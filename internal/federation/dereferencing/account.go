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
	"io"
	"net/url"
	"time"

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

func (d *deref) GetAccountByURI(ctx context.Context, requestUser string, uri *url.URL, block bool) (*gtsmodel.Account, error) {
	var (
		account *gtsmodel.Account
		uriStr  = uri.String()
		err     error
	)

	// Search the database for existing account with ID URI.
	account, err = d.db.GetAccountByURI(ctx, uriStr)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return nil, fmt.Errorf("GetAccountByURI: error checking database for account %s by uri: %w", uriStr, err)
	}

	if account == nil {
		// Else, search the database for existing by ID URL.
		account, err = d.db.GetAccountByURL(ctx, uriStr)
		if err != nil && !errors.Is(err, db.ErrNoEntries) {
			return nil, fmt.Errorf("GetAccountByURI: error checking database for account %s by url: %w", uriStr, err)
		}
	}

	if account == nil {
		// Ensure that this is isn't a search for a local account.
		if uri.Host == config.GetHost() || uri.Host == config.GetAccountDomain() {
			return nil, NewErrNotRetrievable(err) // this will be db.ErrNoEntries
		}

		// Create and pass-through a new bare-bones model for dereferencing.
		return d.enrichAccount(ctx, requestUser, uri, &gtsmodel.Account{
			ID:     id.NewULID(),
			Domain: uri.Host,
			URI:    uriStr,
		}, false, true)
	}

	// Try to update existing account model
	enriched, err := d.enrichAccount(ctx, requestUser, uri, account, false, block)
	if err != nil {
		log.Errorf(ctx, "error enriching remote account: %v", err)
		return account, nil // fall back to returning existing
	}

	return enriched, nil
}

func (d *deref) GetAccountByUsernameDomain(ctx context.Context, requestUser string, username string, domain string, block bool) (*gtsmodel.Account, error) {
	if domain == config.GetHost() || domain == config.GetAccountDomain() {
		// We do local lookups using an empty domain,
		// else it will fail the db search below.
		domain = ""
	}

	// Search the database for existing account with USERNAME@DOMAIN
	account, err := d.db.GetAccountByUsernameDomain(ctx, username, domain)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return nil, fmt.Errorf("GetAccountByUsernameDomain: error checking database for account %s@%s: %w", username, domain, err)
	}

	if account == nil {
		// Check for failed local lookup.
		if domain == "" {
			return nil, NewErrNotRetrievable(err) // will be db.ErrNoEntries
		}

		// Create and pass-through a new bare-bones model for dereferencing.
		return d.enrichAccount(ctx, requestUser, nil, &gtsmodel.Account{
			ID:       id.NewULID(),
			Username: username,
			Domain:   domain,
		}, false, true)
	}

	// Try to update existing account model
	enriched, err := d.enrichAccount(ctx, requestUser, nil, account, false, block)
	if err != nil {
		log.Errorf(ctx, "error enriching account from remote: %v", err)
		return account, nil // fall back to returning unchanged existing account model
	}

	return enriched, nil
}

func (d *deref) UpdateAccount(ctx context.Context, requestUser string, account *gtsmodel.Account, force bool) (*gtsmodel.Account, error) {
	return d.enrichAccount(ctx, requestUser, nil, account, force, false)
}

// enrichAccount will ensure the given account is the most up-to-date model of the account, re-webfingering and re-dereferencing if necessary.
func (d *deref) enrichAccount(ctx context.Context, requestUser string, uri *url.URL, account *gtsmodel.Account, force, block bool) (*gtsmodel.Account, error) {
	if account.IsLocal() {
		// Can't update local accounts.
		return account, nil
	}

	if !account.CreatedAt.IsZero() && account.IsInstance() {
		// Existing instance account. No need for update.
		return account, nil
	}

	if !force {
		const interval = time.Hour * 48

		// If this account was updated recently (last interval), we return as-is.
		if next := account.FetchedAt.Add(interval); time.Now().Before(next) {
			return account, nil
		}
	}

	// Pre-fetch a transport for requesting username, used by later deref procedures.
	transport, err := d.transportController.NewTransportForUsername(ctx, requestUser)
	if err != nil {
		return nil, fmt.Errorf("enrichAccount: couldn't create transport: %w", err)
	}

	if account.Username != "" {
		// A username was provided so we can attempt a webfinger, this ensures up-to-date accountdomain info.
		accDomain, accURI, err := d.fingerRemoteAccount(ctx, transport, account.Username, account.Domain)

		switch {
		case err != nil && account.URI == "":
			// this is a new account (to us) with username@domain but failed webfinger, nothing more we can do.
			return nil, fmt.Errorf("enrichAccount: error webfingering account: %w", err)

		case err != nil:
			log.Errorf(ctx, "error webfingering[1] remote account %s@%s: %v", account.Username, account.Domain, err)

		case err == nil:
			if account.Domain != accDomain {
				// After webfinger, we now have correct account domain from which we can do a final DB check.
				alreadyAccount, err := d.db.GetAccountByUsernameDomain(ctx, account.Username, accDomain)
				if err != nil && !errors.Is(err, db.ErrNoEntries) {
					return nil, fmt.Errorf("enrichAccount: db err looking for account again after webfinger: %w", err)
				}

				if err == nil {
					// Enrich existing account.
					account = alreadyAccount
				}
			}

			// Update account with latest info.
			account.URI = accURI.String()
			account.Domain = accDomain
			uri = accURI
		}
	}

	if uri == nil {
		var err error

		// No URI provided / found, must parse from account.
		uri, err = url.Parse(account.URI)
		if err != nil {
			return nil, fmt.Errorf("enrichAccount: invalid uri %q: %w", account.URI, err)
		}
	}

	// Check whether this account URI is a blocked domain / subdomain.
	if blocked, err := d.db.IsDomainBlocked(ctx, uri.Host); err != nil {
		return nil, newErrDB(fmt.Errorf("enrichAccount: error checking blocked domain: %w", err))
	} else if blocked {
		return nil, fmt.Errorf("enrichAccount: %s is blocked", uri.Host)
	}

	// Mark deref+update handshake start.
	d.startHandshake(requestUser, uri)
	defer d.stopHandshake(requestUser, uri)

	// Dereference this account to get the latest available.
	apubAcc, err := d.dereferenceAccountable(ctx, transport, uri)
	if err != nil {
		return nil, fmt.Errorf("enrichAccount: error dereferencing account %s: %w", uri, err)
	}

	// Convert the dereferenced AP account object to our GTS model.
	latestAcc, err := d.typeConverter.ASRepresentationToAccount(
		ctx, apubAcc, account.Domain,
	)
	if err != nil {
		return nil, fmt.Errorf("enrichAccount: error converting accountable to gts model for account %s: %w", uri, err)
	}

	if account.Username == "" {
		// No username was provided, so no webfinger was attempted earlier.
		//
		// Now we have a username we can attempt again, to ensure up-to-date
		// accountDomain info. For this final attempt we should use the domain
		// of the ID of the dereffed account, rather than the URI we were given.
		//
		// This avoids cases where we were given a URI like
		// https://example.org/@someone@somewhere.else and we've been redirected
		// from example.org to somewhere.else: we want to take somewhere.else
		// as the accountDomain then, not the example.org we were redirected from.

		// Assume the host from the returned ActivityPub representation.
		idProp := apubAcc.GetJSONLDId()
		if idProp == nil || !idProp.IsIRI() {
			return nil, errors.New("enrichAccount: no id property found on person, or id was not an iri")
		}
		accHost := idProp.GetIRI().Host

		accDomain, _, err := d.fingerRemoteAccount(ctx, transport, latestAcc.Username, accHost)
		if err != nil {
			// We still couldn't webfinger the account, so we're not certain
			// what the accountDomain actually is. Still, we can make a solid
			// guess that it's the Host of the ActivityPub URI of the account.
			// If we're wrong, we can just try again in a couple days.
			log.Errorf(ctx, "error webfingering[2] remote account %s@%s: %v", latestAcc.Username, accHost, err)
			latestAcc.Domain = accHost
		} else {
			// Update account with latest info.
			latestAcc.Domain = accDomain
		}
	}

	// Ensure ID is set and update fetch time.
	latestAcc.ID = account.ID
	latestAcc.FetchedAt = time.Now()

	// Use the existing account media attachments by default.
	latestAcc.AvatarMediaAttachmentID = account.AvatarMediaAttachmentID
	latestAcc.HeaderMediaAttachmentID = account.HeaderMediaAttachmentID

	if latestAcc.AvatarRemoteURL != account.AvatarRemoteURL {
		// Reset the avatar media ID (handles removed).
		latestAcc.AvatarMediaAttachmentID = ""

		if latestAcc.AvatarRemoteURL != "" {
			// Avatar has changed to a new one, fetch up-to-date copy and use new ID.
			latestAcc.AvatarMediaAttachmentID, err = d.fetchRemoteAccountAvatar(ctx,
				transport,
				latestAcc.AvatarRemoteURL,
				latestAcc.ID,
			)
			if err != nil {
				log.Errorf(ctx, "error fetching remote avatar for account %s: %v", uri, err)

				// Keep old avatar for now, we'll try again in $interval.
				latestAcc.AvatarMediaAttachmentID = account.AvatarMediaAttachmentID
				latestAcc.AvatarRemoteURL = account.AvatarRemoteURL
			}
		}
	}

	if latestAcc.HeaderRemoteURL != account.HeaderRemoteURL {
		// Reset the header media ID (handles removed).
		latestAcc.HeaderMediaAttachmentID = ""

		if latestAcc.HeaderRemoteURL != "" {
			// Header has changed to a new one, fetch up-to-date copy and use new ID.
			latestAcc.HeaderMediaAttachmentID, err = d.fetchRemoteAccountHeader(ctx,
				transport,
				latestAcc.HeaderRemoteURL,
				latestAcc.ID,
			)
			if err != nil {
				log.Errorf(ctx, "error fetching remote header for account %s: %v", uri, err)

				// Keep old header for now, we'll try again in $interval.
				latestAcc.HeaderMediaAttachmentID = account.HeaderMediaAttachmentID
				latestAcc.HeaderRemoteURL = account.HeaderRemoteURL
			}
		}
	}

	// Fetch the latest remote account emoji IDs used in account display name/bio.
	if _, err = d.fetchRemoteAccountEmojis(ctx, latestAcc, requestUser); err != nil {
		log.Errorf(ctx, "error fetching remote emojis for account %s: %v", uri, err)
	}

	if account.CreatedAt.IsZero() {
		// CreatedAt will be zero if no local copy was
		// found in one of the GetAccountBy___() functions.
		//
		// Set time of creation from the last-fetched date.
		latestAcc.CreatedAt = latestAcc.FetchedAt
		latestAcc.UpdatedAt = latestAcc.FetchedAt

		// This is new, put it in the database.
		err := d.db.PutAccount(ctx, latestAcc)

		if errors.Is(err, db.ErrAlreadyExists) {
			// TODO: replace this quick fix with per-URI deref locks.
			latestAcc, err = d.db.GetAccountByURI(ctx, latestAcc.URI)
		}

		if err != nil {
			return nil, fmt.Errorf("enrichAccount: error putting in database: %w", err)
		}
	} else {
		// Set time of update from the last-fetched date.
		latestAcc.UpdatedAt = latestAcc.FetchedAt

		// Use existing account values.
		latestAcc.CreatedAt = account.CreatedAt
		latestAcc.Language = account.Language

		// This is an existing account, update the model in the database.
		if err := d.db.UpdateAccount(ctx, latestAcc); err != nil {
			return nil, fmt.Errorf("enrichAccount: error updating database: %w", err)
		}
	}

	if latestAcc.FeaturedCollectionURI != "" {
		// Fetch this account's pinned statuses, now that the account is in the database.
		//
		// The order is important here: if we tried to fetch the pinned statuses before
		// storing the account, the process might end up calling enrichAccount again,
		// causing us to get stuck in a loop. By calling it now, we make sure this doesn't
		// happen!
		if err := d.fetchRemoteAccountFeatured(ctx, requestUser, latestAcc.FeaturedCollectionURI, latestAcc.ID); err != nil {
			log.Errorf(ctx, "error fetching featured collection for account %s: %v", uri, err)
		}
	}

	return latestAcc, nil
}

// dereferenceAccountable calls remoteAccountID with a GET request, and tries to parse whatever
// it finds as something that an account model can be constructed out of.
//
// Will work for Person, Application, or Service models.
func (d *deref) dereferenceAccountable(ctx context.Context, transport transport.Transport, remoteAccountID *url.URL) (ap.Accountable, error) {
	b, err := transport.Dereference(ctx, remoteAccountID)
	if err != nil {
		return nil, fmt.Errorf("DereferenceAccountable: error deferencing %s: %w", remoteAccountID.String(), err)
	}

	m := make(map[string]interface{})
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("DereferenceAccountable: error unmarshalling bytes into json: %w", err)
	}

	t, err := streams.ToType(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("DereferenceAccountable: error resolving json into ap vocab type: %w", err)
	}

	//nolint:forcetypeassert
	switch t.GetTypeName() {
	case ap.ActorApplication:
		return t.(vocab.ActivityStreamsApplication), nil
	case ap.ActorGroup:
		return t.(vocab.ActivityStreamsGroup), nil
	case ap.ActorOrganization:
		return t.(vocab.ActivityStreamsOrganization), nil
	case ap.ActorPerson:
		return t.(vocab.ActivityStreamsPerson), nil
	case ap.ActorService:
		return t.(vocab.ActivityStreamsService), nil
	}

	return nil, newErrWrongType(fmt.Errorf("DereferenceAccountable: type name %s not supported as Accountable", t.GetTypeName()))
}

func (d *deref) fetchRemoteAccountAvatar(ctx context.Context, tsport transport.Transport, avatarURL string, accountID string) (string, error) {
	// Parse and validate provided media URL.
	avatarURI, err := url.Parse(avatarURL)
	if err != nil {
		return "", err
	}

	// Acquire lock for derefs map.
	unlock := d.derefAvatarsMu.Lock()
	defer unlock()

	// Look for an existing dereference in progress.
	processing, ok := d.derefAvatars[avatarURL]

	if !ok {
		var err error

		// Set the media data function to dereference avatar from URI.
		data := func(ctx context.Context) (io.ReadCloser, int64, error) {
			return tsport.DereferenceMedia(ctx, avatarURI)
		}

		// Create new media processing request from the media manager instance.
		processing, err = d.mediaManager.PreProcessMedia(ctx, data, nil, accountID, &media.AdditionalMediaInfo{
			Avatar:    func() *bool { v := true; return &v }(),
			RemoteURL: &avatarURL,
		})
		if err != nil {
			return "", err
		}

		// Store media in map to mark as processing.
		d.derefAvatars[avatarURL] = processing

		defer func() {
			// On exit safely remove media from map.
			unlock := d.derefAvatarsMu.Lock()
			delete(d.derefAvatars, avatarURL)
			unlock()
		}()
	}

	// Unlock map.
	unlock()

	// Start media attachment loading (blocking call).
	if _, err := processing.LoadAttachment(ctx); err != nil {
		return "", err
	}

	return processing.AttachmentID(), nil
}

func (d *deref) fetchRemoteAccountHeader(ctx context.Context, tsport transport.Transport, headerURL string, accountID string) (string, error) {
	// Parse and validate provided media URL.
	headerURI, err := url.Parse(headerURL)
	if err != nil {
		return "", err
	}

	// Acquire lock for derefs map.
	unlock := d.derefHeadersMu.Lock()
	defer unlock()

	// Look for an existing dereference in progress.
	processing, ok := d.derefHeaders[headerURL]

	if !ok {
		var err error

		// Set the media data function to dereference header from URI.
		data := func(ctx context.Context) (io.ReadCloser, int64, error) {
			return tsport.DereferenceMedia(ctx, headerURI)
		}

		// Create new media processing request from the media manager instance.
		processing, err = d.mediaManager.PreProcessMedia(ctx, data, nil, accountID, &media.AdditionalMediaInfo{
			Header:    func() *bool { v := true; return &v }(),
			RemoteURL: &headerURL,
		})
		if err != nil {
			return "", err
		}

		// Store media in map to mark as processing.
		d.derefHeaders[headerURL] = processing

		defer func() {
			// On exit safely remove media from map.
			unlock := d.derefHeadersMu.Lock()
			delete(d.derefHeaders, headerURL)
			unlock()
		}()
	}

	// Unlock map.
	unlock()

	// Start media attachment loading (blocking call).
	if _, err := processing.LoadAttachment(ctx); err != nil {
		return "", err
	}

	return processing.AttachmentID(), nil
}

func (d *deref) fetchRemoteAccountEmojis(ctx context.Context, targetAccount *gtsmodel.Account, requestingUsername string) (bool, error) {
	maybeEmojis := targetAccount.Emojis
	maybeEmojiIDs := targetAccount.EmojiIDs

	// It's possible that the account had emoji IDs set on it, but not Emojis
	// themselves, depending on how it was fetched before being passed to us.
	//
	// If we only have IDs, fetch the emojis from the db. We know they're in
	// there or else they wouldn't have IDs.
	if len(maybeEmojiIDs) > len(maybeEmojis) {
		maybeEmojis = make([]*gtsmodel.Emoji, 0, len(maybeEmojiIDs))
		for _, emojiID := range maybeEmojiIDs {
			maybeEmoji, err := d.db.GetEmojiByID(ctx, emojiID)
			if err != nil {
				return false, err
			}
			maybeEmojis = append(maybeEmojis, maybeEmoji)
		}
	}

	// For all the maybe emojis we have, we either fetch them from the database
	// (if we haven't already), or dereference them from the remote instance.
	gotEmojis, err := d.populateEmojis(ctx, maybeEmojis, requestingUsername)
	if err != nil {
		return false, err
	}

	// Extract the ID of each fetched or dereferenced emoji, so we can attach
	// this to the account if necessary.
	gotEmojiIDs := make([]string, 0, len(gotEmojis))
	for _, e := range gotEmojis {
		gotEmojiIDs = append(gotEmojiIDs, e.ID)
	}

	var (
		changed  = false // have the emojis for this account changed?
		maybeLen = len(maybeEmojis)
		gotLen   = len(gotEmojis)
	)

	// if the length of everything is zero, this is simple:
	// nothing has changed and there's nothing to do
	if maybeLen == 0 && gotLen == 0 {
		return changed, nil
	}

	// if the *amount* of emojis on the account has changed, then the got emojis
	// are definitely different from the previous ones (if there were any) --
	// the account has either more or fewer emojis set on it now, so take the
	// discovered emojis as the new correct ones.
	if maybeLen != gotLen {
		changed = true
		targetAccount.Emojis = gotEmojis
		targetAccount.EmojiIDs = gotEmojiIDs
		return changed, nil
	}

	// if the lengths are the same but not all of the slices are
	// zero, something *might* have changed, so we have to check

	// 1. did we have emojis before that we don't have now?
	for _, maybeEmoji := range maybeEmojis {
		var stillPresent bool

		for _, gotEmoji := range gotEmojis {
			if maybeEmoji.URI == gotEmoji.URI {
				// the emoji we maybe had is still present now,
				// so we can stop checking gotEmojis
				stillPresent = true
				break
			}
		}

		if !stillPresent {
			// at least one maybeEmoji is no longer present in
			// the got emojis, so we can stop checking now
			changed = true
			targetAccount.Emojis = gotEmojis
			targetAccount.EmojiIDs = gotEmojiIDs
			return changed, nil
		}
	}

	// 2. do we have emojis now that we didn't have before?
	for _, gotEmoji := range gotEmojis {
		var wasPresent bool

		for _, maybeEmoji := range maybeEmojis {
			// check emoji IDs here as well, because unreferenced
			// maybe emojis we didn't already have would not have
			// had IDs set on them yet
			if gotEmoji.URI == maybeEmoji.URI && gotEmoji.ID == maybeEmoji.ID {
				// this got emoji was present already in the maybeEmoji,
				// so we can stop checking through maybeEmojis
				wasPresent = true
				break
			}
		}

		if !wasPresent {
			// at least one gotEmojis was not present in
			// the maybeEmojis, so we can stop checking now
			changed = true
			targetAccount.Emojis = gotEmojis
			targetAccount.EmojiIDs = gotEmojiIDs
			return changed, nil
		}
	}

	return changed, nil
}

// fetchRemoteAccountFeatured dereferences an account's featuredCollectionURI (if not empty).
// For each discovered status, this status will be dereferenced (if necessary) and marked as
// pinned (if necessary). Then, old pins will be removed if they're not included in new pins.
func (d *deref) fetchRemoteAccountFeatured(ctx context.Context, requestingUsername string, featuredCollectionURI string, accountID string) error {
	uri, err := url.Parse(featuredCollectionURI)
	if err != nil {
		return err
	}

	tsport, err := d.transportController.NewTransportForUsername(ctx, requestingUsername)
	if err != nil {
		return err
	}

	b, err := tsport.Dereference(ctx, uri)
	if err != nil {
		return err
	}

	m := make(map[string]interface{})
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("error unmarshalling bytes into json: %w", err)
	}

	t, err := streams.ToType(ctx, m)
	if err != nil {
		return fmt.Errorf("error resolving json into ap vocab type: %w", err)
	}

	if t.GetTypeName() != ap.ObjectOrderedCollection {
		return fmt.Errorf("%s was not an OrderedCollection", featuredCollectionURI)
	}

	collection, ok := t.(vocab.ActivityStreamsOrderedCollection)
	if !ok {
		return errors.New("couldn't coerce OrderedCollection")
	}

	items := collection.GetActivityStreamsOrderedItems()
	if items == nil {
		return errors.New("nil orderedItems")
	}

	// Get previous pinned statuses (we'll need these later).
	wasPinned, err := d.db.GetAccountPinnedStatuses(ctx, accountID)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return fmt.Errorf("error getting account pinned statuses: %w", err)
	}

	statusURIs := make([]*url.URL, 0, items.Len())
	for iter := items.Begin(); iter != items.End(); iter = iter.Next() {
		var statusURI *url.URL

		switch {
		case iter.IsActivityStreamsNote():
			// We got a whole Note. Extract the URI.
			if note := iter.GetActivityStreamsNote(); note != nil {
				if id := note.GetJSONLDId(); id != nil {
					statusURI = id.GetIRI()
				}
			}
		case iter.IsActivityStreamsArticle():
			// We got a whole Article. Extract the URI.
			if article := iter.GetActivityStreamsArticle(); article != nil {
				if id := article.GetJSONLDId(); id != nil {
					statusURI = id.GetIRI()
				}
			}
		default:
			// Try to get just the URI.
			statusURI = iter.GetIRI()
		}

		if statusURI == nil {
			continue
		}

		if statusURI.Host != uri.Host {
			// If this status doesn't share a host with its featured
			// collection URI, we shouldn't trust it. Just move on.
			continue
		}

		// Already append this status URI to our slice.
		// We do this here so that even if we can't get
		// the status in the next part for some reason,
		// we still know it was *meant* to be pinned.
		statusURIs = append(statusURIs, statusURI)

		status, _, err := d.GetStatus(ctx, requestingUsername, statusURI, false, false)
		if err != nil {
			// We couldn't get the status, bummer.
			// Just log + move on, we can try later.
			log.Errorf(ctx, "error getting status from featured collection %s: %s", featuredCollectionURI, err)
			continue
		}

		// If the status was already pinned, we don't need to do anything.
		if !status.PinnedAt.IsZero() {
			continue
		}

		if status.AccountID != accountID {
			// Someone's pinned a status that doesn't
			// belong to them, this doesn't work for us.
			continue
		}

		if status.BoostOfID != "" {
			// Someone's pinned a boost. This also
			// doesn't work for us.
			continue
		}

		// All conditions are met for this status to
		// be pinned, so we can finally update it.
		status.PinnedAt = time.Now()
		if err := d.db.UpdateStatus(ctx, status, "pinned_at"); err != nil {
			log.Errorf(ctx, "error updating status in featured collection %s: %s", featuredCollectionURI, err)
		}
	}

	// Now that we know which statuses are pinned, we should
	// *unpin* previous pinned statuses that aren't included.
outerLoop:
	for _, status := range wasPinned {
		for _, statusURI := range statusURIs {
			if status.URI == statusURI.String() {
				// This status is included in most recent
				// pinned uris. No need to keep checking.
				continue outerLoop
			}
		}

		// Status was pinned before, but is not included
		// in most recent pinned uris, so unpin it now.
		status.PinnedAt = time.Time{}
		if err := d.db.UpdateStatus(ctx, status, "pinned_at"); err != nil {
			return fmt.Errorf("error unpinning status: %w", err)
		}
	}

	return nil
}
