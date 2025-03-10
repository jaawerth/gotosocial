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

package status

import (
	"context"
	"errors"
	"fmt"

	apimodel "github.com/superseriousbusiness/gotosocial/internal/api/model"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/id"
)

// BookmarkCreate adds a bookmark for the requestingAccount, targeting the given status (no-op if bookmark already exists).
func (p *Processor) BookmarkCreate(ctx context.Context, requestingAccount *gtsmodel.Account, targetStatusID string) (*apimodel.Status, gtserror.WithCode) {
	targetStatus, err := p.state.DB.GetStatusByID(ctx, targetStatusID)
	if err != nil {
		return nil, gtserror.NewErrorNotFound(fmt.Errorf("error fetching status %s: %s", targetStatusID, err))
	}
	if targetStatus.Account == nil {
		return nil, gtserror.NewErrorNotFound(fmt.Errorf("no status owner for status %s", targetStatusID))
	}
	visible, err := p.filter.StatusVisible(ctx, targetStatus, requestingAccount)
	if err != nil {
		return nil, gtserror.NewErrorNotFound(fmt.Errorf("error seeing if status %s is visible: %s", targetStatus.ID, err))
	}
	if !visible {
		return nil, gtserror.NewErrorNotFound(errors.New("status is not visible"))
	}

	// first check if the status is already bookmarked, if so we don't need to do anything
	newBookmark := true
	gtsBookmark := &gtsmodel.StatusBookmark{}
	if err := p.state.DB.GetWhere(ctx, []db.Where{{Key: "status_id", Value: targetStatus.ID}, {Key: "account_id", Value: requestingAccount.ID}}, gtsBookmark); err == nil {
		// we already have a bookmark for this status
		newBookmark = false
	}

	if newBookmark {
		// we need to create a new bookmark in the database
		gtsBookmark := &gtsmodel.StatusBookmark{
			ID:              id.NewULID(),
			AccountID:       requestingAccount.ID,
			Account:         requestingAccount,
			TargetAccountID: targetStatus.AccountID,
			TargetAccount:   targetStatus.Account,
			StatusID:        targetStatus.ID,
			Status:          targetStatus,
		}

		if err := p.state.DB.Put(ctx, gtsBookmark); err != nil {
			return nil, gtserror.NewErrorInternalError(fmt.Errorf("error putting bookmark in database: %s", err))
		}
	}

	// return the apidon representation of the target status
	apiStatus, err := p.tc.StatusToAPIStatus(ctx, targetStatus, requestingAccount)
	if err != nil {
		return nil, gtserror.NewErrorInternalError(fmt.Errorf("error converting status %s to frontend representation: %s", targetStatus.ID, err))
	}

	return apiStatus, nil
}

// BookmarkRemove removes a bookmark for the requesting account, targeting the given status (no-op if bookmark doesn't exist).
func (p *Processor) BookmarkRemove(ctx context.Context, requestingAccount *gtsmodel.Account, targetStatusID string) (*apimodel.Status, gtserror.WithCode) {
	targetStatus, err := p.state.DB.GetStatusByID(ctx, targetStatusID)
	if err != nil {
		return nil, gtserror.NewErrorNotFound(fmt.Errorf("error fetching status %s: %s", targetStatusID, err))
	}
	if targetStatus.Account == nil {
		return nil, gtserror.NewErrorNotFound(fmt.Errorf("no status owner for status %s", targetStatusID))
	}
	visible, err := p.filter.StatusVisible(ctx, targetStatus, requestingAccount)
	if err != nil {
		return nil, gtserror.NewErrorNotFound(fmt.Errorf("error seeing if status %s is visible: %s", targetStatus.ID, err))
	}
	if !visible {
		return nil, gtserror.NewErrorNotFound(errors.New("status is not visible"))
	}

	// first check if the status is actually bookmarked
	toUnbookmark := false
	gtsBookmark := &gtsmodel.StatusBookmark{}
	if err := p.state.DB.GetWhere(ctx, []db.Where{{Key: "status_id", Value: targetStatus.ID}, {Key: "account_id", Value: requestingAccount.ID}}, gtsBookmark); err == nil {
		// we have a bookmark for this status
		toUnbookmark = true
	}

	if toUnbookmark {
		if err := p.state.DB.DeleteWhere(ctx, []db.Where{{Key: "status_id", Value: targetStatus.ID}, {Key: "account_id", Value: requestingAccount.ID}}, gtsBookmark); err != nil {
			return nil, gtserror.NewErrorInternalError(fmt.Errorf("error unfaveing status: %s", err))
		}
	}

	// return the api representation of the target status
	apiStatus, err := p.tc.StatusToAPIStatus(ctx, targetStatus, requestingAccount)
	if err != nil {
		return nil, gtserror.NewErrorInternalError(fmt.Errorf("error converting status %s to frontend representation: %s", targetStatus.ID, err))
	}

	return apiStatus, nil
}
