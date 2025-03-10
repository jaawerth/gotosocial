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
	"time"

	apimodel "github.com/superseriousbusiness/gotosocial/internal/api/model"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
)

const allowedPinnedCount = 10

// getPinnableStatus fetches targetStatusID status and ensures that requestingAccountID
// can pin or unpin it.
//
// It checks:
//   - Status is visible to requesting account.
//   - Status belongs to requesting account.
//   - Status is public, unlisted, or followers-only.
//   - Status is not a boost.
func (p *Processor) getPinnableStatus(ctx context.Context, targetStatusID string, requestingAccountID string) (*gtsmodel.Status, gtserror.WithCode) {
	targetStatus, err := p.state.DB.GetStatusByID(ctx, targetStatusID)
	if err != nil {
		err = fmt.Errorf("error fetching status %s: %w", targetStatusID, err)
		return nil, gtserror.NewErrorNotFound(err)
	}

	requestingAccount, err := p.state.DB.GetAccountByID(ctx, requestingAccountID)
	if err != nil {
		return nil, gtserror.NewErrorInternalError(err)
	}

	visible, err := p.filter.StatusVisible(ctx, targetStatus, requestingAccount)
	if err != nil {
		return nil, gtserror.NewErrorInternalError(err)
	}

	if !visible {
		err = fmt.Errorf("status %s not visible to account %s", targetStatusID, requestingAccountID)
		return nil, gtserror.NewErrorNotFound(err)
	}

	if targetStatus.AccountID != requestingAccountID {
		err = fmt.Errorf("status %s does not belong to account %s", targetStatusID, requestingAccountID)
		return nil, gtserror.NewErrorUnprocessableEntity(err, err.Error())
	}

	if targetStatus.Visibility == gtsmodel.VisibilityDirect {
		err = errors.New("cannot pin direct messages")
		return nil, gtserror.NewErrorUnprocessableEntity(err, err.Error())
	}

	if targetStatus.BoostOfID != "" {
		err = errors.New("cannot pin boosts")
		return nil, gtserror.NewErrorUnprocessableEntity(err, err.Error())
	}

	return targetStatus, nil
}

// PinCreate pins the target status to the top of requestingAccount's profile, if possible.
//
// Conditions for a pin to work:
//   - Status belongs to requesting account.
//   - Status is public, unlisted, or followers-only.
//   - Status is not a boost.
//   - Status is not already pinnd.
//   - Limit of pinned statuses not yet met or exceeded.
//
// If the conditions can't be met, then code 422 Unprocessable Entity will be returned.
func (p *Processor) PinCreate(ctx context.Context, requestingAccount *gtsmodel.Account, targetStatusID string) (*apimodel.Status, gtserror.WithCode) {
	targetStatus, errWithCode := p.getPinnableStatus(ctx, targetStatusID, requestingAccount.ID)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if !targetStatus.PinnedAt.IsZero() {
		err := errors.New("status already pinned")
		return nil, gtserror.NewErrorUnprocessableEntity(err, err.Error())
	}

	pinnedCount, err := p.state.DB.CountAccountPinned(ctx, requestingAccount.ID)
	if err != nil {
		return nil, gtserror.NewErrorInternalError(fmt.Errorf("error checking number of pinned statuses: %w", err))
	}

	if pinnedCount >= allowedPinnedCount {
		err = fmt.Errorf("status pin limit exceeded, you've already pinned %d status(es) out of %d", pinnedCount, allowedPinnedCount)
		return nil, gtserror.NewErrorUnprocessableEntity(err, err.Error())
	}

	targetStatus.PinnedAt = time.Now()
	if err := p.state.DB.UpdateStatus(ctx, targetStatus, "pinned_at"); err != nil {
		return nil, gtserror.NewErrorInternalError(fmt.Errorf("db error pinning status: %w", err))
	}

	apiStatus, err := p.tc.StatusToAPIStatus(ctx, targetStatus, requestingAccount)
	if err != nil {
		return nil, gtserror.NewErrorInternalError(fmt.Errorf("error converting status %s to frontend representation: %w", targetStatus.ID, err))
	}

	return apiStatus, nil
}

// PinRemove unpins the target status from the top of requestingAccount's profile, if possible.
//
// Conditions for an unpin to work:
//   - Status belongs to requesting account.
//   - Status is public, unlisted, or followers-only.
//   - Status is not a boost.
//
// If the conditions can't be met, then code 422 Unprocessable Entity will be returned.
//
// Unlike with PinCreate, statuses that are already unpinned will not return 422, but just do
// nothing and return the api model representation of the status, to conform to the masto API.
func (p *Processor) PinRemove(ctx context.Context, requestingAccount *gtsmodel.Account, targetStatusID string) (*apimodel.Status, gtserror.WithCode) {
	targetStatus, errWithCode := p.getPinnableStatus(ctx, targetStatusID, requestingAccount.ID)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if !targetStatus.PinnedAt.IsZero() {
		targetStatus.PinnedAt = time.Time{}
		if err := p.state.DB.UpdateStatus(ctx, targetStatus, "pinned_at"); err != nil {
			return nil, gtserror.NewErrorInternalError(fmt.Errorf("db error unpinning status: %w", err))
		}
	}

	apiStatus, err := p.tc.StatusToAPIStatus(ctx, targetStatus, requestingAccount)
	if err != nil {
		return nil, gtserror.NewErrorInternalError(fmt.Errorf("error converting status %s to frontend representation: %w", targetStatus.ID, err))
	}

	return apiStatus, nil
}
