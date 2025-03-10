// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package dereferencing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/superseriousbusiness/activity/streams"
	"github.com/superseriousbusiness/activity/streams/vocab"
	"github.com/superseriousbusiness/gotosocial/internal/ap"
	"github.com/superseriousbusiness/gotosocial/internal/config"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/id"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/media"
	"github.com/superseriousbusiness/gotosocial/internal/transport"
)

// EnrichRemoteStatus takes a remote status that's already been inserted into the database in a minimal form,
// and populates it with additional fields, media, etc.
//
// EnrichRemoteStatus is mostly useful for calling after a status has been initially created by
// the federatingDB's Create function, but additional dereferencing is needed on it.
func (d *deref) EnrichRemoteStatus(ctx context.Context, username string, status *gtsmodel.Status, includeParent bool) (*gtsmodel.Status, error) {
	if err := d.populateStatusFields(ctx, status, username, includeParent); err != nil {
		return nil, err
	}
	if err := d.db.UpdateStatus(ctx, status); err != nil {
		return nil, err
	}
	return status, nil
}

// GetStatus completely dereferences a status, converts it to a GtS model status,
// puts it in the database, and returns it to a caller.
//
// If refetch is true, then regardless of whether we have the original status in the database or not,
// the ap.Statusable representation of the status will be dereferenced and returned.
//
// If refetch is false, the ap.Statusable will only be returned if this is a new status, so callers
// should check whether or not this is nil.
//
// GetAccount will guard against trying to do http calls to fetch a status that belongs to this instance.
// Instead of making calls, it will just return the status early if it finds it, or return an error.
func (d *deref) GetStatus(ctx context.Context, username string, statusURI *url.URL, refetch, includeParent bool) (*gtsmodel.Status, ap.Statusable, error) {
	uriString := statusURI.String()

	// try to get by URI first
	status, dbErr := d.db.GetStatusByURI(ctx, uriString)
	if dbErr != nil {
		if !errors.Is(dbErr, db.ErrNoEntries) {
			// real error
			return nil, nil, newErrDB(fmt.Errorf("GetRemoteStatus: error during GetStatusByURI for %s: %w", uriString, dbErr))
		}
		// no problem, just press on
	} else if !refetch {
		// we already had the status and we aren't being asked to refetch the AP representation
		return status, nil, nil
	}

	// try to get by URL if we couldn't get by URI now
	if status == nil {
		status, dbErr = d.db.GetStatusByURL(ctx, uriString)
		if dbErr != nil {
			if !errors.Is(dbErr, db.ErrNoEntries) {
				// real error
				return nil, nil, newErrDB(fmt.Errorf("GetRemoteStatus: error during GetStatusByURI for %s: %w", uriString, dbErr))
			}
			// no problem, just press on
		} else if !refetch {
			// we already had the status and we aren't being asked to refetch the AP representation
			return status, nil, nil
		}
	}

	// guard against having our own statuses passed in
	if host := statusURI.Host; host == config.GetHost() || host == config.GetAccountDomain() {
		// this is our status, definitely don't search for it
		if status != nil {
			return status, nil, nil
		}
		return nil, nil, NewErrNotRetrievable(fmt.Errorf("GetRemoteStatus: uri %s is apparently ours, but we have nothing in the db for it, will not proceed to dereference our own status", uriString))
	}

	// if we got here, either we didn't have the status
	// in the db, or we had it but need to refetch it
	tsport, err := d.transportController.NewTransportForUsername(ctx, username)
	if err != nil {
		return nil, nil, newErrTransportError(fmt.Errorf("GetRemoteStatus: error creating transport for %s: %w", username, err))
	}

	statusable, derefErr := d.dereferenceStatusable(ctx, tsport, statusURI)
	if derefErr != nil {
		return nil, nil, wrapDerefError(derefErr, "GetRemoteStatus: error dereferencing statusable")
	}

	if status != nil && refetch {
		// we already had the status in the db, and we've also
		// now fetched the AP representation as requested
		return status, statusable, nil
	}

	// from here on out we can consider this to be a 'new' status because we didn't have the status in the db already
	accountURI, err := ap.ExtractAttributedTo(statusable)
	if err != nil {
		return nil, nil, newErrOther(fmt.Errorf("GetRemoteStatus: error extracting attributedTo: %w", err))
	}

	// we need to get the author of the status else we can't serialize it properly
	if _, err = d.GetAccountByURI(ctx, username, accountURI, true); err != nil {
		return nil, nil, newErrOther(fmt.Errorf("GetRemoteStatus: couldn't get status author: %s", err))
	}

	status, err = d.typeConverter.ASStatusToStatus(ctx, statusable)
	if err != nil {
		return nil, nil, newErrOther(fmt.Errorf("GetRemoteStatus: error converting statusable to status: %s", err))
	}

	ulid, err := id.NewULIDFromTime(status.CreatedAt)
	if err != nil {
		return nil, nil, newErrOther(fmt.Errorf("GetRemoteStatus: error generating new id for status: %s", err))
	}
	status.ID = ulid

	if err := d.populateStatusFields(ctx, status, username, includeParent); err != nil {
		return nil, nil, newErrOther(fmt.Errorf("GetRemoteStatus: error populating status fields: %s", err))
	}

	if err := d.db.PutStatus(ctx, status); err != nil && !errors.Is(err, db.ErrAlreadyExists) {
		return nil, nil, newErrDB(fmt.Errorf("GetRemoteStatus: error putting new status: %s", err))
	}

	return status, statusable, nil
}

func (d *deref) dereferenceStatusable(ctx context.Context, tsport transport.Transport, remoteStatusID *url.URL) (ap.Statusable, error) {
	if blocked, err := d.db.IsDomainBlocked(ctx, remoteStatusID.Host); blocked || err != nil {
		return nil, fmt.Errorf("DereferenceStatusable: domain %s is blocked", remoteStatusID.Host)
	}

	b, err := tsport.Dereference(ctx, remoteStatusID)
	if err != nil {
		return nil, fmt.Errorf("DereferenceStatusable: error deferencing %s: %s", remoteStatusID.String(), err)
	}

	m := make(map[string]interface{})
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("DereferenceStatusable: error unmarshalling bytes into json: %s", err)
	}

	t, err := streams.ToType(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("DereferenceStatusable: error resolving json into ap vocab type: %s", err)
	}

	// Article, Document, Image, Video, Note, Page, Event, Place, Mention, Profile
	switch t.GetTypeName() {
	case ap.ObjectArticle:
		p, ok := t.(vocab.ActivityStreamsArticle)
		if !ok {
			return nil, errors.New("DereferenceStatusable: error resolving type as ActivityStreamsArticle")
		}
		return p, nil
	case ap.ObjectDocument:
		p, ok := t.(vocab.ActivityStreamsDocument)
		if !ok {
			return nil, errors.New("DereferenceStatusable: error resolving type as ActivityStreamsDocument")
		}
		return p, nil
	case ap.ObjectImage:
		p, ok := t.(vocab.ActivityStreamsImage)
		if !ok {
			return nil, errors.New("DereferenceStatusable: error resolving type as ActivityStreamsImage")
		}
		return p, nil
	case ap.ObjectVideo:
		p, ok := t.(vocab.ActivityStreamsVideo)
		if !ok {
			return nil, errors.New("DereferenceStatusable: error resolving type as ActivityStreamsVideo")
		}
		return p, nil
	case ap.ObjectNote:
		p, ok := t.(vocab.ActivityStreamsNote)
		if !ok {
			return nil, errors.New("DereferenceStatusable: error resolving type as ActivityStreamsNote")
		}
		return p, nil
	case ap.ObjectPage:
		p, ok := t.(vocab.ActivityStreamsPage)
		if !ok {
			return nil, errors.New("DereferenceStatusable: error resolving type as ActivityStreamsPage")
		}
		return p, nil
	case ap.ObjectEvent:
		p, ok := t.(vocab.ActivityStreamsEvent)
		if !ok {
			return nil, errors.New("DereferenceStatusable: error resolving type as ActivityStreamsEvent")
		}
		return p, nil
	case ap.ObjectPlace:
		p, ok := t.(vocab.ActivityStreamsPlace)
		if !ok {
			return nil, errors.New("DereferenceStatusable: error resolving type as ActivityStreamsPlace")
		}
		return p, nil
	case ap.ObjectProfile:
		p, ok := t.(vocab.ActivityStreamsProfile)
		if !ok {
			return nil, errors.New("DereferenceStatusable: error resolving type as ActivityStreamsProfile")
		}
		return p, nil
	}

	return nil, newErrWrongType(fmt.Errorf("DereferenceStatusable: type name %s not supported as Statusable", t.GetTypeName()))
}

// populateStatusFields fetches all the information we temporarily pinned to an incoming
// federated status, back in the federating db's Create function.
//
// When a status comes in from the federation API, there are certain fields that
// haven't been dereferenced yet, because we needed to provide a snappy synchronous
// response to the caller. By the time it reaches this function though, it's being
// processed asynchronously, so we have all the time in the world to fetch the various
// bits and bobs that are attached to the status, and properly flesh it out, before we
// send the status to any timelines and notify people.
//
// Things to dereference and fetch here:
//
// 1. Media attachments.
// 2. Hashtags.
// 3. Emojis.
// 4. Mentions.
// 5. Replied-to-status.
//
// SIDE EFFECTS:
// This function will deference all of the above, insert them in the database as necessary,
// and attach them to the status. The status itself will not be added to the database yet,
// that's up the caller to do.
func (d *deref) populateStatusFields(ctx context.Context, status *gtsmodel.Status, requestingUsername string, includeParent bool) error {
	statusIRI, err := url.Parse(status.URI)
	if err != nil {
		return fmt.Errorf("populateStatusFields: couldn't parse status URI %s: %s", status.URI, err)
	}

	blocked, err := d.db.IsURIBlocked(ctx, statusIRI)
	if err != nil {
		return fmt.Errorf("populateStatusFields: error checking blocked status of %s: %s", statusIRI, err)
	}
	if blocked {
		return fmt.Errorf("populateStatusFields: domain %s is blocked", statusIRI)
	}

	// in case the status doesn't have an id yet (ie., it hasn't entered the database yet), then create one
	if status.ID == "" {
		newID, err := id.NewULIDFromTime(status.CreatedAt)
		if err != nil {
			return fmt.Errorf("populateStatusFields: error creating ulid for status: %s", err)
		}
		status.ID = newID
	}

	// 1. Media attachments.
	if err := d.populateStatusAttachments(ctx, status, requestingUsername); err != nil {
		return fmt.Errorf("populateStatusFields: error populating status attachments: %s", err)
	}

	// 2. Hashtags
	// TODO

	// 3. Emojis
	if err := d.populateStatusEmojis(ctx, status, requestingUsername); err != nil {
		return fmt.Errorf("populateStatusFields: error populating status emojis: %s", err)
	}

	// 4. Mentions
	// TODO: do we need to handle removing empty mention objects and just using mention IDs slice?
	if err := d.populateStatusMentions(ctx, status, requestingUsername); err != nil {
		return fmt.Errorf("populateStatusFields: error populating status mentions: %s", err)
	}

	// 5. Replied-to-status (only if requested)
	if includeParent {
		if err := d.populateStatusRepliedTo(ctx, status, requestingUsername); err != nil {
			return fmt.Errorf("populateStatusFields: error populating status repliedTo: %s", err)
		}
	}

	return nil
}

func (d *deref) populateStatusMentions(ctx context.Context, status *gtsmodel.Status, requestingUsername string) error {
	// At this point, mentions should have the namestring and mentionedAccountURI set on them.
	// We can use these to find the accounts.

	mentionIDs := []string{}
	newMentions := []*gtsmodel.Mention{}
	for _, m := range status.Mentions {
		if m.ID != "" {
			// we've already populated this mention, since it has an ID
			log.Debug(ctx, "mention already populated")
			mentionIDs = append(mentionIDs, m.ID)
			newMentions = append(newMentions, m)
			continue
		}

		if m.TargetAccountURI == "" {
			log.Debug(ctx, "target URI not set on mention")
			continue
		}

		targetAccountURI, err := url.Parse(m.TargetAccountURI)
		if err != nil {
			log.Debugf(ctx, "error parsing mentioned account uri %s: %s", m.TargetAccountURI, err)
			continue
		}

		var targetAccount *gtsmodel.Account
		errs := []string{}

		// check if account is in the db already
		if a, err := d.db.GetAccountByURI(ctx, targetAccountURI.String()); err != nil {
			errs = append(errs, err.Error())
		} else {
			log.Debugf(ctx, "got target account %s with id %s through GetAccountByURI", targetAccountURI, a.ID)
			targetAccount = a
		}

		if targetAccount == nil {
			// we didn't find the account in our database already
			// check if we can get the account remotely (dereference it)
			if a, err := d.GetAccountByURI(ctx, requestingUsername, targetAccountURI, false); err != nil {
				errs = append(errs, err.Error())
			} else {
				log.Debugf(ctx, "got target account %s with id %s through GetRemoteAccount", targetAccountURI, a.ID)
				targetAccount = a
			}
		}

		if targetAccount == nil {
			log.Debugf(ctx, "couldn't get target account %s: %s", m.TargetAccountURI, strings.Join(errs, " : "))
			continue
		}

		mID, err := id.NewRandomULID()
		if err != nil {
			return fmt.Errorf("populateStatusMentions: error generating ulid: %s", err)
		}

		newMention := &gtsmodel.Mention{
			ID:               mID,
			StatusID:         status.ID,
			Status:           m.Status,
			CreatedAt:        status.CreatedAt,
			UpdatedAt:        status.UpdatedAt,
			OriginAccountID:  status.AccountID,
			OriginAccountURI: status.AccountURI,
			OriginAccount:    status.Account,
			TargetAccountID:  targetAccount.ID,
			TargetAccount:    targetAccount,
			NameString:       m.NameString,
			TargetAccountURI: targetAccount.URI,
			TargetAccountURL: targetAccount.URL,
		}

		if err := d.db.Put(ctx, newMention); err != nil {
			return fmt.Errorf("populateStatusMentions: error creating mention: %s", err)
		}

		mentionIDs = append(mentionIDs, newMention.ID)
		newMentions = append(newMentions, newMention)
	}

	status.MentionIDs = mentionIDs
	status.Mentions = newMentions

	return nil
}

func (d *deref) populateStatusAttachments(ctx context.Context, status *gtsmodel.Status, requestingUsername string) error {
	// At this point we should know:
	// * the media type of the file we're looking for (a.File.ContentType)
	// * the file type (a.Type)
	// * the remote URL (a.RemoteURL)
	// This should be enough to dereference the piece of media.

	attachmentIDs := []string{}
	attachments := []*gtsmodel.MediaAttachment{}

	for _, a := range status.Attachments {
		a.AccountID = status.AccountID
		a.StatusID = status.ID

		processingMedia, err := d.GetRemoteMedia(ctx, requestingUsername, a.AccountID, a.RemoteURL, &media.AdditionalMediaInfo{
			CreatedAt:   &a.CreatedAt,
			StatusID:    &a.StatusID,
			RemoteURL:   &a.RemoteURL,
			Description: &a.Description,
			Blurhash:    &a.Blurhash,
		})
		if err != nil {
			log.Errorf(ctx, "couldn't get remote media %s: %s", a.RemoteURL, err)
			continue
		}

		attachment, err := processingMedia.LoadAttachment(ctx)
		if err != nil {
			log.Errorf(ctx, "couldn't load remote attachment %s: %s", a.RemoteURL, err)
			continue
		}

		attachmentIDs = append(attachmentIDs, attachment.ID)
		attachments = append(attachments, attachment)
	}

	status.AttachmentIDs = attachmentIDs
	status.Attachments = attachments

	return nil
}

func (d *deref) populateStatusEmojis(ctx context.Context, status *gtsmodel.Status, requestingUsername string) error {
	emojis, err := d.populateEmojis(ctx, status.Emojis, requestingUsername)
	if err != nil {
		return err
	}

	emojiIDs := make([]string, 0, len(emojis))
	for _, e := range emojis {
		emojiIDs = append(emojiIDs, e.ID)
	}

	status.Emojis = emojis
	status.EmojiIDs = emojiIDs
	return nil
}

func (d *deref) populateStatusRepliedTo(ctx context.Context, status *gtsmodel.Status, requestingUsername string) error {
	if status.InReplyToURI != "" && status.InReplyToID == "" {
		statusURI, err := url.Parse(status.InReplyToURI)
		if err != nil {
			return err
		}

		replyToStatus, _, err := d.GetStatus(ctx, requestingUsername, statusURI, false, false)
		if err != nil {
			return fmt.Errorf("populateStatusRepliedTo: couldn't get reply to status with uri %s: %s", status.InReplyToURI, err)
		}

		// we have the status
		status.InReplyToID = replyToStatus.ID
		status.InReplyTo = replyToStatus
		status.InReplyToAccountID = replyToStatus.AccountID
		status.InReplyToAccount = replyToStatus.Account
	}

	return nil
}
