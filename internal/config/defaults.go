/*
   GoToSocial
   Copyright (C) 2021-2022 GoToSocial Authors admin@gotosocial.org

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package config

import (
	"time"

	"codeberg.org/gruf/go-bytesize"
	"github.com/coreos/go-oidc/v3/oidc"
)

// Defaults contains a populated Configuration with reasonable defaults. Note that
// if you use this, you will still need to set Host.
var Defaults = Configuration{
	LogLevel:        "info",
	LogDbQueries:    false,
	ApplicationName: "gotosocial",
	LandingPageUser: "",
	ConfigPath:      "config.yaml",
	Host:            "",
	AccountDomain:   "",
	Protocol:        "https",
	BindAddress:     "0.0.0.0",
	Port:            8080,
	TrustedProxies:  []string{"127.0.0.1/32", "::1"}, // localhost

	DbType:      "postgres",
	DbAddress:   "",
	DbPort:      5432,
	DbUser:      "",
	DbPassword:  "",
	DbDatabase:  "gotosocial",
	DbTLSMode:   "disable",
	DbTLSCACert: "",

	WebTemplateBaseDir: "./web/template/",
	WebAssetBaseDir:    "./web/assets/",

	InstanceExposePeers:            false,
	InstanceExposeSuspended:        false,
	InstanceDeliverToSharedInboxes: true,

	AccountsRegistrationOpen: true,
	AccountsApprovalRequired: true,
	AccountsReasonRequired:   true,
	AccountsAllowCustomCSS:   false,

	MediaImageMaxSize:        10 * bytesize.MiB,
	MediaVideoMaxSize:        40 * bytesize.MiB,
	MediaDescriptionMinChars: 0,
	MediaDescriptionMaxChars: 500,
	MediaRemoteCacheDays:     30,
	MediaEmojiLocalMaxSize:   50 * bytesize.KiB,
	MediaEmojiRemoteMaxSize:  100 * bytesize.KiB,

	StorageBackend:       "local",
	StorageLocalBasePath: "/gotosocial/storage",
	StorageS3UseSSL:      true,
	StorageS3Proxy:       false,

	StatusesMaxChars:           5000,
	StatusesCWMaxChars:         100,
	StatusesPollMaxOptions:     6,
	StatusesPollOptionMaxChars: 50,
	StatusesMediaMaxFiles:      6,

	LetsEncryptEnabled:      false,
	LetsEncryptPort:         80,
	LetsEncryptCertDir:      "/gotosocial/storage/certs",
	LetsEncryptEmailAddress: "",

	OIDCEnabled:          false,
	OIDCIdpName:          "",
	OIDCSkipVerification: false,
	OIDCIssuer:           "",
	OIDCClientID:         "",
	OIDCClientSecret:     "",
	OIDCScopes:           []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	OIDCLinkExisting:     false,

	SMTPHost:     "",
	SMTPPort:     0,
	SMTPUsername: "",
	SMTPPassword: "",
	SMTPFrom:     "GoToSocial",

	SyslogEnabled:  false,
	SyslogProtocol: "udp",
	SyslogAddress:  "localhost:514",

	AdvancedCookiesSamesite:   "lax",
	AdvancedRateLimitRequests: 1000, // per 5 minutes

	Cache: CacheConfiguration{
		GTS: GTSCacheConfiguration{
			AccountMaxSize:   100,
			AccountTTL:       time.Minute * 5,
			AccountSweepFreq: time.Second * 10,

			BlockMaxSize:   100,
			BlockTTL:       time.Minute * 5,
			BlockSweepFreq: time.Second * 10,

			DomainBlockMaxSize:   1000,
			DomainBlockTTL:       time.Hour * 24,
			DomainBlockSweepFreq: time.Minute,

			EmojiMaxSize:   500,
			EmojiTTL:       time.Minute * 5,
			EmojiSweepFreq: time.Second * 10,

			EmojiCategoryMaxSize:   100,
			EmojiCategoryTTL:       time.Minute * 5,
			EmojiCategorySweepFreq: time.Second * 10,

			MentionMaxSize:   500,
			MentionTTL:       time.Minute * 5,
			MentionSweepFreq: time.Second * 10,

			NotificationMaxSize:   500,
			NotificationTTL:       time.Minute * 5,
			NotificationSweepFreq: time.Second * 10,

			StatusMaxSize:   500,
			StatusTTL:       time.Minute * 5,
			StatusSweepFreq: time.Second * 10,

			TombstoneMaxSize:   100,
			TombstoneTTL:       time.Minute * 5,
			TombstoneSweepFreq: time.Second * 10,

			UserMaxSize:   100,
			UserTTL:       time.Minute * 5,
			UserSweepFreq: time.Second * 10,
		},
	},
}
