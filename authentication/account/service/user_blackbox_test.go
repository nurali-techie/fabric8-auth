package service_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fabric8-services/admin-console/auditlog"
	factorymanager "github.com/fabric8-services/fabric8-auth/application/factory/manager"
	"github.com/fabric8-services/fabric8-auth/application/service/factory"
	"github.com/fabric8-services/fabric8-auth/authentication/account/repository"
	userservice "github.com/fabric8-services/fabric8-auth/authentication/account/service"
	"github.com/fabric8-services/fabric8-auth/authentication/provider"
	"github.com/fabric8-services/fabric8-auth/authorization/token"
	"github.com/fabric8-services/fabric8-auth/authorization/token/manager"
	"github.com/fabric8-services/fabric8-auth/errors"
	"github.com/fabric8-services/fabric8-auth/gormtestsupport"
	"github.com/fabric8-services/fabric8-auth/notification"
	"github.com/fabric8-services/fabric8-auth/rest"
	testsupport "github.com/fabric8-services/fabric8-auth/test"
	servicemock "github.com/fabric8-services/fabric8-auth/test/generated/application/service"
	userservicemock "github.com/fabric8-services/fabric8-auth/test/generated/authentication/account/service"
	"github.com/fabric8-services/fabric8-auth/test/graph"
	testtoken "github.com/fabric8-services/fabric8-auth/test/token"
	"github.com/fabric8-services/fabric8-common/gocksupport"
	testsuite "github.com/fabric8-services/fabric8-common/test/suite"

	"github.com/dgrijalva/jwt-go"
	"github.com/jinzhu/gorm"
	errs "github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"gopkg.in/h2non/gock.v1"
)

func TestUserService(t *testing.T) {
	suite.Run(t, &userServiceBlackboxTestSuite{
		DBTestSuite: gormtestsupport.NewDBTestSuite(),
	})
}

type userServiceBlackboxTestSuite struct {
	gormtestsupport.DBTestSuite
}

func (s *userServiceBlackboxTestSuite) TestNotifyIdentitiesBeforeDeactivation() {
	ctx := context.Background()
	config := userservicemock.NewUserServiceConfigurationMock(s.T())
	config.GetUserDeactivationInactivityPeriodFunc = func() time.Duration {
		return 31 * 24 * time.Hour // 31 days
	}
	now := time.Now() // make sure we use the same 'now' everywhere in the test
	nowf := func() time.Time {
		return now
	}
	// configure the `SetupSubtest` and `TearDownSubtest` to setup/reset data after each subtest
	var identity1, identity2, identity3 repository.Identity
	var user1, user2 repository.User
	s.SetupSubtest = func() {
		s.CleanTest = testsuite.DeleteCreatedEntities(s.DB, s.Configuration)
		yesterday := time.Now().Add(-1 * 24 * time.Hour)
		ago65days := time.Now().Add(-65 * 24 * time.Hour) // 65 days since last activity and notified...
		ago40days := time.Now().Add(-40 * 24 * time.Hour) // 40 days since last activity and notified...
		ago70days := time.Now().Add(-70 * 24 * time.Hour) // 70 days since last activity and notified...
		// user/identity1: 40 days since last activity and not notified
		user1 = *s.Graph.CreateUser().User()
		identity1 = user1.Identities[0]
		identity1.LastActive = &ago40days
		err := s.Application.Identities().Save(ctx, &identity1)
		require.NoError(s.T(), err)
		// user/identity2: 70 days since last activity and not notified
		user2 = *s.Graph.CreateUser().User()
		identity2 = user2.Identities[0]
		identity2.LastActive = &ago70days
		err = s.Application.Identities().Save(ctx, &identity2)
		require.NoError(s.T(), err)
		// noise: user/identity: 1 day since last activity and not notified yet
		user3 := s.Graph.CreateUser().User()
		s.Graph.CreateIdentity(yesterday)
		identity3 = user3.Identities[0]
		identity3.LastActive = &yesterday
		err = s.Application.Identities().Save(ctx, &identity3)
		require.NoError(s.T(), err)
		// noise: user/identity: 65 days since last activity but banned
		user4 := s.Graph.CreateUser().User()
		identity4 := user4.Identities[0]
		identity4.LastActive = &ago65days
		err = s.Application.Identities().Save(ctx, &identity4)
		require.NoError(s.T(), err)
		user4.Banned = true
		err = s.Application.Users().Save(ctx, user4)
		require.NoError(s.T(), err)
	}
	s.TearDownSubtest = func() {
		err := s.CleanTest()
		require.NoError(s.T(), err)
	}

	s.Run("no user to notify", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 90 * 24 * time.Hour // 90 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{}
		}
		notificationServiceMock := servicemock.NewNotificationServiceMock(s.T())
		notificationServiceMock.SendMessageFunc = func(ctx context.Context, msg notification.Message, options ...rest.HTTPClientOption) error {
			return nil
		}
		adminConsoleServiceMock := servicemock.NewAdminConsoleServiceMock(s.T())
		adminConsoleServiceMock.CreateAuditLogFunc = func(ctx context.Context, username string, eventType string) error {
			return nil
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil, factory.WithNotificationService(notificationServiceMock), factory.WithAdminConsoleService(adminConsoleServiceMock)), config)
		// when
		result, err := userSvc.NotifyIdentitiesBeforeDeactivation(ctx, nowf)
		// then
		require.NoError(s.T(), err)
		assert.Empty(s.T(), result)
		assert.Equal(s.T(), uint64(0), notificationServiceMock.SendMessageCounter)
		assert.Equal(s.T(), uint64(0), adminConsoleServiceMock.CreateAuditLogCounter)
	})

	s.Run("one user to notify without limit", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 60 * 24 * time.Hour // 60 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{}
		}
		var msgToSend notification.Message
		notificationServiceMock := servicemock.NewNotificationServiceMock(s.T())
		notificationServiceMock.SendMessageFunc = func(ctx context.Context, msg notification.Message, options ...rest.HTTPClientOption) error {
			msgToSend = msg
			return nil
		}
		var usernameToSend, eventTypeToSend string
		adminConsoleServiceMock := servicemock.NewAdminConsoleServiceMock(s.T())
		adminConsoleServiceMock.CreateAuditLogFunc = func(ctx context.Context, username string, eventType string) error {
			usernameToSend = username
			eventTypeToSend = eventType
			// The deactivation notification should pass even if the audit log failed
			return errors.NewInternalErrorFromString("oopsie woopsie")
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil, factory.WithNotificationService(notificationServiceMock), factory.WithAdminConsoleService(adminConsoleServiceMock)), config)
		// when
		result, err := userSvc.NotifyIdentitiesBeforeDeactivation(ctx, nowf)
		// then
		require.NoError(s.T(), err)
		require.Len(s.T(), result, 1)
		assert.Equal(s.T(), identity2.ID, result[0].ID)
		assert.Equal(s.T(), uint64(1), notificationServiceMock.SendMessageCounter)
		assert.Equal(s.T(), uint64(1), adminConsoleServiceMock.CreateAuditLogCounter)
		// also check that the `DeactivationNotification` field was set in the DB
		identity, err := s.Application.Identities().Load(ctx, identity2.ID)
		require.NoError(s.T(), err)
		require.NotNil(s.T(), identity.DeactivationNotification)
		assert.True(s.T(), time.Now().Sub(*identity.DeactivationNotification) < time.Second*2)
		// also verify that the message to send to the user has the correct data
		assert.Equal(s.T(), identity2.ID.String(), msgToSend.TargetID)
		expiryDate := userservice.GetExpiryDate(config, nowf)
		assert.Equal(s.T(), expiryDate, msgToSend.Custom["expiryDate"])
		assert.Equal(s.T(), user2.Email, msgToSend.Custom["userEmail"])
		assert.Equal(s.T(), identity2.Username, usernameToSend)
		assert.Equal(s.T(), auditlog.UserDeactivationNotificationEvent, eventTypeToSend)

	})

	s.Run("one user to notify with limit reached", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 1
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 30 * 24 * time.Hour // 30 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{}
		}
		notificationServiceMock := servicemock.NewNotificationServiceMock(s.T())
		notificationServiceMock.SendMessageFunc = func(ctx context.Context, msg notification.Message, options ...rest.HTTPClientOption) error {
			return nil
		}
		adminConsoleServiceMock := servicemock.NewAdminConsoleServiceMock(s.T())
		adminConsoleServiceMock.CreateAuditLogFunc = func(ctx context.Context, username string, eventType string) error {
			return nil
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil, factory.WithNotificationService(notificationServiceMock), factory.WithAdminConsoleService(adminConsoleServiceMock)), config)
		// when
		result, err := userSvc.NotifyIdentitiesBeforeDeactivation(ctx, nowf)
		// then
		require.NoError(s.T(), err)
		require.Len(s.T(), result, 1)
		assert.Equal(s.T(), identity2.ID, result[0].ID)
		assert.Equal(s.T(), uint64(1), notificationServiceMock.SendMessageCounter)
		assert.Equal(s.T(), uint64(1), adminConsoleServiceMock.CreateAuditLogCounter)
		// also check that the `DeactivationNotification` field was set in the DB
		identity, err := s.Application.Identities().Load(ctx, identity2.ID)
		require.NoError(s.T(), err)
		require.NotNil(s.T(), identity.DeactivationNotification)
		assert.True(s.T(), time.Now().Sub(*identity.DeactivationNotification) < time.Second*2)
	})

	s.Run("two users to notify", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 30 * 24 * time.Hour // 30 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{}
		}
		var msgToSend []notification.Message
		notificationServiceMock := servicemock.NewNotificationServiceMock(s.T())
		notificationServiceMock.SendMessageFunc = func(ctx context.Context, msg notification.Message, options ...rest.HTTPClientOption) error {
			msgToSend = append(msgToSend, msg)
			return nil
		}
		var usernameToSend, eventTypeToSend []string
		adminConsoleServiceMock := servicemock.NewAdminConsoleServiceMock(s.T())
		adminConsoleServiceMock.CreateAuditLogFunc = func(ctx context.Context, username string, eventType string) error {
			usernameToSend = append(usernameToSend, username)
			eventTypeToSend = append(eventTypeToSend, eventType)
			return nil
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil, factory.WithNotificationService(notificationServiceMock), factory.WithAdminConsoleService(adminConsoleServiceMock)), config)
		// when
		result, err := userSvc.NotifyIdentitiesBeforeDeactivation(ctx, nowf)
		// then
		require.NoError(s.T(), err)
		require.Len(s.T(), result, 2)
		assert.Equal(s.T(), identity2.ID, result[0].ID)
		assert.Equal(s.T(), identity1.ID, result[1].ID)
		assert.Equal(s.T(), uint64(2), notificationServiceMock.SendMessageCounter)
		assert.Equal(s.T(), uint64(2), adminConsoleServiceMock.CreateAuditLogCounter)
		// also check that the `DeactivationNotification` fields were set for both identities in the DB
		expiryDate := userservice.GetExpiryDate(config, nowf)
		customs := []map[string]interface{}{}
		targetIDs := []string{}
		for _, msg := range msgToSend {
			targetIDs = append(targetIDs, msg.TargetID)
		}
		require.Len(s.T(), msgToSend, 2)
		for i, id := range []uuid.UUID{identity1.ID, identity2.ID} {
			identity, err := s.Application.Identities().Load(ctx, id)
			require.NoError(s.T(), err)
			require.NotNil(s.T(), identity.DeactivationNotification)
			assert.True(s.T(), time.Now().Sub(*identity.DeactivationNotification) < time.Second*2)
			// also verify that the message to send to the user has the correct data
			require.Contains(s.T(), targetIDs, identity.ID.String())
			customs = append(customs, msgToSend[i].Custom)
		}
		// verify that 2 messages were sent, although, we can't be sure in which order
		assert.ElementsMatch(s.T(), customs, []map[string]interface{}{
			{
				"expiryDate": expiryDate,
				"userEmail":  user1.Email,
			},
			{
				"expiryDate": expiryDate,
				"userEmail":  user2.Email,
			},
		})
		// check audit logs creations
		assert.ElementsMatch(s.T(), []string{identity1.Username, identity2.Username}, usernameToSend)
		assert.ElementsMatch(s.T(), []string{auditlog.UserDeactivationNotificationEvent, auditlog.UserDeactivationNotificationEvent}, eventTypeToSend)
	})

	s.Run("two users to notify but one whitelisted", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 30 * 24 * time.Hour // 30 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{identity1.Username}
		}
		var msgToSend []notification.Message
		notificationServiceMock := servicemock.NewNotificationServiceMock(s.T())
		notificationServiceMock.SendMessageFunc = func(ctx context.Context, msg notification.Message, options ...rest.HTTPClientOption) error {
			msgToSend = append(msgToSend, msg)
			return nil
		}
		var usernameToSend, eventTypeToSend []string
		adminConsoleServiceMock := servicemock.NewAdminConsoleServiceMock(s.T())
		adminConsoleServiceMock.CreateAuditLogFunc = func(ctx context.Context, username string, eventType string) error {
			usernameToSend = append(usernameToSend, username)
			eventTypeToSend = append(eventTypeToSend, eventType)
			return nil
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil, factory.WithNotificationService(notificationServiceMock), factory.WithAdminConsoleService(adminConsoleServiceMock)), config)
		// when
		result, err := userSvc.NotifyIdentitiesBeforeDeactivation(ctx, nowf)
		// then
		require.NoError(s.T(), err)
		require.Len(s.T(), result, 1)
		assert.Equal(s.T(), identity2.ID, result[0].ID)
		assert.Equal(s.T(), uint64(1), notificationServiceMock.SendMessageCounter)
		assert.Equal(s.T(), uint64(1), adminConsoleServiceMock.CreateAuditLogCounter)
	})

	s.Run("two users to notify but both whitelisted", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 30 * 24 * time.Hour // 30 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{identity1.Username, identity2.Username}
		}
		var msgToSend []notification.Message
		notificationServiceMock := servicemock.NewNotificationServiceMock(s.T())
		notificationServiceMock.SendMessageFunc = func(ctx context.Context, msg notification.Message, options ...rest.HTTPClientOption) error {
			msgToSend = append(msgToSend, msg)
			return nil
		}
		var usernameToSend, eventTypeToSend []string
		adminConsoleServiceMock := servicemock.NewAdminConsoleServiceMock(s.T())
		adminConsoleServiceMock.CreateAuditLogFunc = func(ctx context.Context, username string, eventType string) error {
			usernameToSend = append(usernameToSend, username)
			eventTypeToSend = append(eventTypeToSend, eventType)
			return nil
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil, factory.WithNotificationService(notificationServiceMock), factory.WithAdminConsoleService(adminConsoleServiceMock)), config)
		// when
		result, err := userSvc.NotifyIdentitiesBeforeDeactivation(ctx, nowf)
		// then
		require.NoError(s.T(), err)
		require.Empty(s.T(), result)
		assert.Equal(s.T(), uint64(0), notificationServiceMock.SendMessageCounter)
		assert.Equal(s.T(), uint64(0), adminConsoleServiceMock.CreateAuditLogCounter)
	})

	s.Run("error while sending second notification", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 30 * 24 * time.Hour
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{}
		}
		notificationServiceMock := servicemock.NewNotificationServiceMock(s.T())
		notificationServiceMock.SendMessageFunc = func(ctx context.Context, msg notification.Message, options ...rest.HTTPClientOption) error {
			if msg.UserID != nil && *msg.UserID == identity2.ID.String() {
				return fmt.Errorf("mock error!")
			}
			return nil
		}
		adminConsoleServiceMock := servicemock.NewAdminConsoleServiceMock(s.T())
		adminConsoleServiceMock.CreateAuditLogFunc = func(ctx context.Context, username string, eventType string) error {
			return nil
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil, factory.WithNotificationService(notificationServiceMock), factory.WithAdminConsoleService(adminConsoleServiceMock)), config)
		// when
		result, err := userSvc.NotifyIdentitiesBeforeDeactivation(ctx, time.Now)
		// then
		require.NoError(s.T(), err)
		require.Len(s.T(), result, 2)
		assert.Equal(s.T(), identity2.ID, result[0].ID)
		assert.Equal(s.T(), identity1.ID, result[1].ID)
		assert.Equal(s.T(), uint64(2), notificationServiceMock.SendMessageCounter)
		assert.Equal(s.T(), uint64(2), adminConsoleServiceMock.CreateAuditLogCounter)
		// also check that the `DeactivationNotification` fields were NOT set for identity #2 in the DB
		identity, err := s.Application.Identities().Load(ctx, identity2.ID)
		require.NoError(s.T(), err)
		require.Nil(s.T(), identity.DeactivationNotification)
		// but it worked for identity #1
		identity, err = s.Application.Identities().Load(ctx, identity1.ID)
		require.NoError(s.T(), err)
		require.NotNil(s.T(), identity.DeactivationNotification)
		assert.True(s.T(), time.Now().Sub(*identity.DeactivationNotification) < time.Second*2)
	})

}
func (s *userServiceBlackboxTestSuite) TestListUsersToDeactivate() {
	config := userservicemock.NewUserServiceConfigurationMock(s.T())
	config.GetUserDeactivationInactivityPeriodFunc = func() time.Duration {
		return 31 * 24 * time.Hour // 31 days
	}
	ctx := context.Background()
	yesterday := time.Now().Add(-1 * 24 * time.Hour)
	ago3days := time.Now().Add(-3 * 24 * time.Hour)
	ago10days := time.Now().Add(-10 * 24 * time.Hour)
	ago65days := time.Now().Add(-65 * 24 * time.Hour) // 65 days since last activity and notified...
	ago40days := time.Now().Add(-40 * 24 * time.Hour) // 40 days since last activity and notified...
	ago70days := time.Now().Add(-70 * 24 * time.Hour) // 70 days since last activity and notified...
	// user/identity1: 40 days since last activity and notified
	user1 := s.Graph.CreateUser().User()
	identity1 := user1.Identities[0]
	identity1.LastActive = &ago40days
	identity1.DeactivationNotification = &ago10days
	identity1.DeactivationScheduled = &ago3days
	err := s.Application.Identities().Save(ctx, &identity1)
	require.NoError(s.T(), err)
	// user/identity2: 70 days since last activity and notified
	user2 := s.Graph.CreateUser().User()
	identity2 := user2.Identities[0]
	identity2.LastActive = &ago70days
	identity2.DeactivationNotification = &ago10days
	identity2.DeactivationScheduled = &ago3days
	err = s.Application.Identities().Save(ctx, &identity2)
	require.NoError(s.T(), err)
	// noise: user/identity: 1 day since last activity and not notified yet
	user3 := s.Graph.CreateUser().User()
	s.Graph.CreateIdentity(time.Now().Add(-24 * time.Hour))
	identity3 := user3.Identities[0]
	identity3.LastActive = &yesterday
	err = s.Application.Identities().Save(ctx, &identity3)
	require.NoError(s.T(), err)
	// noise: user/identity: 65 days since last activity and notified, but also banned
	user4 := s.Graph.CreateUser().User()
	identity4 := user4.Identities[0]
	identity4.LastActive = &ago65days
	identity4.DeactivationNotification = &yesterday
	err = s.Application.Identities().Save(ctx, &identity4)
	require.NoError(s.T(), err)
	user4.Banned = true
	err = s.Application.Users().Save(ctx, user4)
	require.NoError(s.T(), err)

	s.Run("no user to deactivate", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityPeriodFunc = func() time.Duration {
			return 90 * 24 * time.Hour // 90 days
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 80 * 24 * time.Hour // 80 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{}
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil), config)
		// when
		result, err := userSvc.ListIdentitiesToDeactivate(ctx, time.Now)
		// then
		require.NoError(s.T(), err)
		assert.Empty(s.T(), result)
	})

	s.Run("one user to deactivate without limit", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityPeriodFunc = func() time.Duration {
			return 60 * 24 * time.Hour // 60 days
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 55 * 24 * time.Hour // 55 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{}
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil), config)
		// when
		result, err := userSvc.ListIdentitiesToDeactivate(ctx, time.Now)
		// then
		require.NoError(s.T(), err)
		require.Len(s.T(), result, 1)
		assert.Equal(s.T(), identity2.ID, result[0].ID) // user 2 was inactive for 70 days and notified 10 days ago
	})

	s.Run("one user to deactivate with limit reached", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 1
		}
		config.GetUserDeactivationInactivityPeriodFunc = func() time.Duration {
			return 30 * 24 * time.Hour // 30 days
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 20 * 24 * time.Hour // 20 days
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil), config)
		// when
		result, err := userSvc.ListIdentitiesToDeactivate(ctx, time.Now)
		// then
		require.NoError(s.T(), err)
		require.Len(s.T(), result, 1)
		assert.Equal(s.T(), identity2.ID, result[0].ID) // user 2 was inactive for 70 days and notified 10 days ago
	})

	s.Run("two users to deactivate", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityPeriodFunc = func() time.Duration {
			return 30 * 24 * time.Hour // 30 days
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 20 * 24 * time.Hour // 20 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{}
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil), config)
		// when
		result, err := userSvc.ListIdentitiesToDeactivate(ctx, time.Now)
		// then
		require.NoError(s.T(), err)
		require.Len(s.T(), result, 2)
		assert.Equal(s.T(), identity2.ID, result[0].ID) // user 2 was inactive for 70 days and notified 10 days ago
		assert.Equal(s.T(), identity1.ID, result[1].ID) // user 1 was inactive for 40 days and notified 10 days ago
	})

	s.Run("one user excluded from deactivation (even if scheduled)", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityPeriodFunc = func() time.Duration {
			return 30 * 24 * time.Hour // 30 days
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 20 * 24 * time.Hour // 20 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{identity1.Username}
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil), config)
		// when
		result, err := userSvc.ListIdentitiesToDeactivate(ctx, time.Now)
		// then
		require.NoError(s.T(), err)
		require.Len(s.T(), result, 1)                   // user1 was excluded from deactivation
		assert.Equal(s.T(), identity2.ID, result[0].ID) // user 2 was inactive for 70 days and notified 10 days ago
	})

	s.Run("two users excluded from deactivation (even if scheduled)", func() {
		// given
		config.GetUserDeactivationFetchLimitFunc = func() int {
			return 100
		}
		config.GetUserDeactivationInactivityPeriodFunc = func() time.Duration {
			return 30 * 24 * time.Hour // 30 days
		}
		config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
			return 20 * 24 * time.Hour // 20 days
		}
		config.GetUserDeactivationWhiteListFunc = func() []string {
			return []string{identity1.Username, identity2.Username}
		}
		userSvc := userservice.NewUserService(factory.NewServiceContext(s.Application, s.Application, nil, nil), config)
		// when
		result, err := userSvc.ListIdentitiesToDeactivate(ctx, time.Now)
		// then
		require.NoError(s.T(), err)
		require.Empty(s.T(), result) // both user1 and user2 were excluded
	})

}

// Testing workflow of user to notify and deactivate, or not, depending on their activity, etc.
func (s *userServiceBlackboxTestSuite) TestUserDeactivationFlow() {
	// given
	config := userservicemock.NewUserServiceConfigurationMock(s.T())
	config.GetUserDeactivationFetchLimitFunc = func() int {
		return 100
	}
	config.GetUserDeactivationWhiteListFunc = func() []string {
		return []string{}
	}
	config.GetUserDeactivationInactivityPeriodFunc = func() time.Duration {
		return 30 * 24 * time.Hour // 31 days, ie, 7 days after notification
	}
	config.GetUserDeactivationInactivityNotificationPeriodFunc = func() time.Duration {
		return 20 * 24 * time.Hour // 24 days
	}
	ctx := context.Background()
	yesterday := time.Now().Add(-1 * 24 * time.Hour)
	ago40days := time.Now().Add(-40 * 24 * time.Hour) // 40 days since last activity and notified...

	notificationServiceMock := servicemock.NewNotificationServiceMock(s.T())
	notificationServiceMock.SendMessageFunc = func(ctx context.Context, msg notification.Message, options ...rest.HTTPClientOption) error {
		return nil
	}
	adminConsoleServiceMock := servicemock.NewAdminConsoleServiceMock(s.T())
	adminConsoleServiceMock.CreateAuditLogFunc = func(ctx context.Context, username string, eventType string) error {
		return nil
	}

	svcCtx := factory.NewServiceContext(s.Application, s.Application, s.Configuration, factorymanager.NewFactoryWrappers(), factory.WithNotificationService(notificationServiceMock), factory.WithAdminConsoleService(adminConsoleServiceMock))
	userSvc := userservice.NewUserService(svcCtx, config)

	// ----------------------------------------
	// Step 1: User A is not active for 40 days
	// and was not notified yet.
	// ----------------------------------------
	user1 := s.Graph.CreateUser().User()
	identity1 := user1.Identities[0]
	identity1.LastActive = &ago40days
	err := s.Application.Identities().Save(ctx, &identity1)
	require.NoError(s.T(), err)
	// Check if User A is returned to be notified.
	notified, err := userSvc.NotifyIdentitiesBeforeDeactivation(ctx, time.Now)
	require.NoError(s.T(), err)
	require.Len(s.T(), notified, 1)
	assert.Equal(s.T(), identity1.ID, notified[0].ID)
	// Check if User A is not returned to be deactivated yet.
	deactivated, err := userSvc.ListIdentitiesToDeactivate(ctx, time.Now)
	require.NoError(s.T(), err)
	require.Empty(s.T(), deactivated)

	// -----------------------------------------------------------
	// Step 2: Run the notification emitting again (ie, next day),
	// and check that user A is not notified again.
	// -----------------------------------------------------------
	notified, err = userSvc.NotifyIdentitiesBeforeDeactivation(ctx, time.Now)
	require.NoError(s.T(), err)
	require.Empty(s.T(), notified)

	// ----------------------------------
	// Step 3: User A does some activity.
	// ----------------------------------
	identity1.LastActive = &yesterday
	err = s.Application.Identities().Save(ctx, &identity1)
	require.NoError(s.T(), err)
	// Check if User A is not returned to be notified (activity is too recent)
	notified, err = userSvc.NotifyIdentitiesBeforeDeactivation(ctx, time.Now)
	require.NoError(s.T(), err)
	require.Empty(s.T(), notified)
	// Check if User A is not returned to be deactivated yet.
	deactivated, err = userSvc.ListIdentitiesToDeactivate(ctx, time.Now)
	require.NoError(s.T(), err)
	require.Empty(s.T(), deactivated)

	// ------------------------------------------------------------
	// Step 4: After notification period the user A still not among
	// users to be notified nor deactivated: she was notified but
	// came back to the platform in the mean time.
	// ------------------------------------------------------------
	// 10 days later...
	in10days := func() time.Time {
		return time.Now().Add(10 * 24 * time.Hour)
	}
	// Check if User A is not returned to be notified (activity is too recent)
	notified, err = userSvc.NotifyIdentitiesBeforeDeactivation(ctx, in10days)
	require.NoError(s.T(), err)
	require.Empty(s.T(), notified)
	// Check if User A is not returned to be deactivated yet.
	deactivated, err = userSvc.ListIdentitiesToDeactivate(ctx, in10days)
	require.NoError(s.T(), err)
	require.Empty(s.T(), deactivated)
}

func (s *userServiceBlackboxTestSuite) TestBanUser() {

	s.T().Run("ok", func(t *testing.T) {
		userToBan := s.Graph.CreateUser()
		userToStayIntact := s.Graph.CreateUser()

		token1 := s.Graph.CreateToken(userToBan)
		token2 := s.Graph.CreateToken(userToBan)
		token3 := s.Graph.CreateToken(userToStayIntact)
		token4 := s.Graph.CreateToken(userToStayIntact)
		githubTokenToKeep := s.Graph.CreateExternalToken(userToStayIntact, provider.GitHubProviderAlias)
		openshiftTokenToKeep := s.Graph.CreateExternalToken(userToStayIntact, "1234eee5-d01a-4119-9893-292a7d39b49e") // ID of the OpenShift cluster returned by gock on behalf of the cluster service

		ctx, _, reqID := testtoken.ContextWithTokenAndRequestID(s.T())
		ctx = testtoken.ContextWithRequest(ctx)
		saToken := testtoken.TokenManager.AuthServiceAccountToken()

		// call to Tenant Service
		tenantCallsCounter := 0
		defer gock.Off()
		gock.New("http://localhost:8090").
			Delete(fmt.Sprintf("/api/tenants/%s", userToBan.IdentityID().String())).
			MatchHeader("Authorization", "Bearer "+saToken).
			MatchHeader("X-Request-Id", reqID).
			SetMatcher(gocksupport.SpyOnCalls(&tenantCallsCounter)).
			Reply(204)
		// call to Che
		cheCallsCounter := 0
		gock.New("http://localhost:8091").
			Delete(fmt.Sprintf("/api/user/%s", userToBan.IdentityID().String())).
			SetMatcher(gocksupport.SpyOnCalls(&cheCallsCounter)).
			Reply(204)

		tokenManager, err := manager.DefaultManager(s.Configuration)
		require.NoError(s.T(), err)
		tokenMatcher := gock.NewBasicMatcher()
		tokenMatcher.Add(func(req *http.Request, ereq *gock.Request) (bool, error) {
			h := req.Header.Get("Authorization")
			if strings.HasPrefix(h, "Bearer ") {
				token := h[len("Bearer "):]
				// parse the token and check the 'sub' claim
				tk, err := tokenManager.Parse(context.Background(), token)
				if err != nil {
					return false, err
				}
				if claims, ok := tk.Claims.(jwt.MapClaims); ok {
					return claims["sub"] == userToBan.IdentityID().String(), nil
				}
			}
			return false, nil
		})

		identity, err := s.Application.UserService().BanUser(ctx, userToBan.Identity().Username)
		require.NoError(t, err)
		assert.True(t, identity.User.Banned)
		assert.True(t, identity.User.Deprovisioned) // for backward compatibility
		assert.Equal(t, userToBan.User().ID, identity.User.ID)
		assert.Equal(t, userToBan.IdentityID(), identity.ID)

		loadedUser := s.Graph.LoadUser(userToBan.IdentityID())
		assert.True(t, loadedUser.User().Banned)
		userToBan.Identity().User.Banned = true
		testsupport.AssertIdentityEqual(t, userToBan.Identity(), loadedUser.Identity())

		// also, verify that user's tokens were revoked
		for _, tID := range []uuid.UUID{token1.TokenID(), token2.TokenID()} {
			tok := s.Graph.LoadToken(tID)
			require.NotNil(t, tok)
			assert.Equal(t, tok.Token().Status, token.TOKEN_STATUS_REVOKED)
		}
		// also, verify that che, and tenant services were called
		assert.Equal(t, 1, tenantCallsCounter)
		assert.Equal(t, 1, cheCallsCounter)

		// lastly, verify that everything belonging to the user to keep intact remained as-is
		loadedUser = s.Graph.LoadUser(userToStayIntact.IdentityID())
		assert.Equal(t, false, loadedUser.User().Banned)
		assert.Equal(t, false, loadedUser.User().Deprovisioned) // for backward compatibility
		testsupport.AssertIdentityEqual(t, userToStayIntact.Identity(), loadedUser.Identity())
		assert.True(t, loadedUser.User().Active)
		testsupport.AssertIdentityEqual(t, userToStayIntact.Identity(), loadedUser.Identity())
		for _, tID := range []uuid.UUID{token3.TokenID(), token4.TokenID()} {
			tok := s.Graph.LoadToken(tID)
			require.NotNil(t, tok)
			assert.True(t, tok.Token().Valid())
		}
		_, err = s.Application.ExternalTokens().Load(ctx, githubTokenToKeep.ID())
		require.NoError(t, err)
		_, err = s.Application.ExternalTokens().Load(ctx, openshiftTokenToKeep.ID())
		require.NoError(t, err)
	})

	s.T().Run("fail", func(t *testing.T) {

		s.T().Run("unknown user", func(t *testing.T) {
			// given
			username := uuid.NewV4().String()
			// when
			_, err := s.Application.UserService().BanUser(s.Ctx, username)
			// then
			testsupport.AssertError(t, err, errors.NotFoundError{}, "user identity with username '%s' not found", username)

		})

		s.T().Run("tenant fails", func(t *testing.T) {
			userToBan := s.Graph.CreateUser()
			ctx, _, _ := testtoken.ContextWithTokenAndRequestID(s.T())
			ctx = testtoken.ContextWithRequest(ctx)

			// call to Tenant Service
			defer gock.Off()
			gock.New("http://localhost:8090").
				Delete(fmt.Sprintf("/api/tenants/%s", userToBan.IdentityID().String())).
				Reply(500)
			// call to Che
			gock.New("http://localhost:8091").
				Delete(fmt.Sprintf("/api/user/%s", userToBan.IdentityID().String())).
				Reply(204)

			identity, err := s.Application.UserService().BanUser(ctx, userToBan.Identity().Username)
			assert.EqualError(t, err, fmt.Sprintf("error occurred during deleting the user '%s' on Tenant Service: unable to delete tenant", userToBan.Identity().ID.String()))

			// User is marked as banned anyway
			assert.True(s.T(), identity.User.Banned)
		})

		s.T().Run("che fails", func(t *testing.T) {
			userToBan := s.Graph.CreateUser()
			ctx, _, _ := testtoken.ContextWithTokenAndRequestID(s.T())
			ctx = testtoken.ContextWithRequest(ctx)

			// call to Che
			gock.New("http://localhost:8091").
				Delete(fmt.Sprintf("/api/user/%s", userToBan.IdentityID().String())).
				Reply(500)

			identity, err := s.Application.UserService().BanUser(ctx, userToBan.Identity().Username)
			assert.EqualError(t, err, fmt.Sprintf("error occurred during deleting the user '%s' on Che Service: unable to delete user '%s' in Che", userToBan.Identity().ID.String(), userToBan.Identity().ID.String()))

			// User is marked as banned anyway
			assert.True(s.T(), identity.User.Banned)
		})
	})
}

func (s *userServiceBlackboxTestSuite) TestDeactivate() {

	// given
	ctx, _, reqID := testtoken.ContextWithTokenAndRequestID(s.T())
	ctx = testtoken.ContextWithRequest(ctx)

	saToken := testtoken.TokenManager.AuthServiceAccountToken()

	s.T().Run("ok", func(t *testing.T) {
		// given 2 users with tokens
		userToDeactivate := s.Graph.CreateUser()
		token1 := s.Graph.CreateToken(userToDeactivate)
		token2 := s.Graph.CreateToken(userToDeactivate)
		githubTokenToRemove := s.Graph.CreateExternalToken(userToDeactivate, provider.GitHubProviderID)
		openshiftTokenToRemove := s.Graph.CreateExternalToken(userToDeactivate, "33456e01-0ce4-4da2-b94d-daa968412662") // ID of the OpenShift cluster returned by gock on behalf of the cluster service
		userToStayIntact := s.Graph.CreateUser()
		token3 := s.Graph.CreateToken(userToStayIntact)
		token4 := s.Graph.CreateToken(userToStayIntact)
		githubTokenToKeep := s.Graph.CreateExternalToken(userToStayIntact, provider.GitHubProviderAlias)
		openshiftTokenToKeep := s.Graph.CreateExternalToken(userToStayIntact, "02f2eee5-d01a-4119-9893-292a7d39b49e") // ID of the OpenShift cluster returned by gock on behalf of the cluster service

		defer gock.Off()
		// call to Cluster Service
		gock.New("http://f8cluster").
			Get("/api/clusters/auth").
			Reply(200).
			BodyString(
				fmt.Sprintf(`{
					"data": [
						{
							"token-provider-id": "33456e01-0ce4-4da2-b94d-daa968412662",
							"api-url": "%s",
							"app-dns": "a347.foo.openshiftapps.com",
							"auth-client-default-scope": "user:full",
							"auth-client-id": "openshift-io",
							"auth-client-secret": "067da2df-b721-48cd-8e76-ac26e9140218",
							"capacity-exhausted": false,
							"console-url": "https://console.foo.openshift.com/console/",
							"logging-url": "https://console.foo.openshift.com/console/",
							"metrics-url": "https://metrics.foo.openshift.com/",
							"name": "foo",
							"service-account-token": "1d147ba1-2832-4048-b1c5-21ae37377f0d",
							"service-account-username": "devtools-sre"
						},
						{
							"token-provider-id": "02f2eee5-d01a-4119-9893-292a7d39b49e",
							"api-url": "%s",
							"app-dns": "a347.foo.openshiftapps.com",
							"auth-client-default-scope": "user:full",
							"auth-client-id": "openshift-io",
							"auth-client-secret": "90ceb4c9-842a-4a82-8f1b-e2fd2e0117fe",
							"capacity-exhausted": false,
							"console-url": "https://console.foo.openshift.com/console/",
							"logging-url": "https://console.foo.openshift.com/console/",
							"metrics-url": "https://metrics.foo.openshift.com/",
							"name": "foo",
							"service-account-token": "1d147ba1-2832-4048-b1c5-21ae37377f0d",
							"service-account-username": "devtools-sre"
						}
					]
				}`, userToDeactivate.User().Cluster, userToStayIntact.User().Cluster))

		gock.Observe(gock.DumpRequest)

		// call to Tenant Service
		tenantCallsCounter := 0
		gock.New("http://localhost:8090").
			Delete(fmt.Sprintf("/api/tenants/%s", userToDeactivate.IdentityID().String())).
			MatchHeader("Authorization", "Bearer "+saToken).
			MatchHeader("X-Request-Id", reqID).
			SetMatcher(gocksupport.SpyOnCalls(&tenantCallsCounter)).
			Reply(204)
		// call to Che
		cheCallsCounter := 0
		gock.New("http://localhost:8091").
			Delete(fmt.Sprintf("/api/user/%s", userToDeactivate.IdentityID().String())).
			SetMatcher(gocksupport.SpyOnCalls(&cheCallsCounter)).
			Reply(204)

		tokenManager, err := manager.DefaultManager(s.Configuration)
		require.NoError(s.T(), err)
		tokenMatcher := gock.NewBasicMatcher()
		tokenMatcher.Add(func(req *http.Request, ereq *gock.Request) (bool, error) {
			h := req.Header.Get("Authorization")
			if strings.HasPrefix(h, "Bearer ") {
				token := h[len("Bearer "):]
				// parse the token and check the 'sub' claim
				tk, err := tokenManager.Parse(context.Background(), token)
				if err != nil {
					return false, err
				}
				if claims, ok := tk.Claims.(jwt.MapClaims); ok {
					return claims["sub"] == userToDeactivate.IdentityID().String(), nil
				}
			}
			return false, nil
		})
		adminConsoleServiceMock := servicemock.NewAdminConsoleServiceMock(s.T())
		var auditlogUsername string
		adminConsoleServiceMock.CreateAuditLogFunc = func(ctx context.Context, username string, eventType string) error {
			// capture the username to make sure it's the non-obfuscated value that is used when submitting the request
			// to the admin console service.
			auditlogUsername = username
			// The deactivation should pass even if the audit log failed
			return errors.NewInternalErrorFromString("oopsie woopsie")
		}
		svcCtx := factory.NewServiceContext(s.Application, s.Application, s.Configuration, factorymanager.NewFactoryWrappers(), factory.WithAdminConsoleService(adminConsoleServiceMock))
		userSvc := userservice.NewUserService(svcCtx, s.Configuration)

		// when
		identity, err := userSvc.DeactivateUser(ctx, userToDeactivate.Identity().Username)
		// then
		require.NoError(t, err)
		assert.False(t, identity.User.Active) // user is inactive...
		assert.False(t, identity.User.Banned) // ... but user NOT banned
		assert.Equal(t, userToDeactivate.User().ID, identity.User.ID)
		assert.Equal(t, userToDeactivate.IdentityID(), identity.ID)
		// verify that user's fields were obfuscated and that the record was soft-deleted
		loadedUser := s.Graph.LoadUser(userToDeactivate.IdentityID(), graph.Unscoped())
		require.NotNil(t, loadedUser)
		testsupport.AssertIdentityObfuscated(t, userToDeactivate.Identity(), loadedUser.Identity())
		testsupport.AssertIdentitySoftDeleted(t, loadedUser.Identity())
		// also, verify that user's tokens were revoked
		for _, tID := range []uuid.UUID{token1.TokenID(), token2.TokenID()} {
			tok := s.Graph.LoadToken(tID)
			require.NotNil(t, tok)
			assert.Equal(t, tok.Token().Status, token.TOKEN_STATUS_REVOKED)
		}
		// also, verify that che, and tenant services were called
		assert.Equal(t, 1, tenantCallsCounter)
		assert.Equal(t, 1, cheCallsCounter)
		// also, verify that the external accounts where unlinked
		_, err = s.Application.ExternalTokens().Load(ctx, githubTokenToRemove.ID())
		testsupport.AssertError(t, err, errors.NotFoundError{}, fmt.Sprintf("external_token with id '%s' not found", githubTokenToRemove.ID()))
		_, err = s.Application.ExternalTokens().Load(ctx, openshiftTokenToRemove.ID())
		testsupport.AssertError(t, err, errors.NotFoundError{}, fmt.Sprintf("external_token with id '%s' not found", openshiftTokenToRemove.ID()))
		// also, verify that Auditlog with created with the non-obfuscated username
		assert.Equal(t, userToDeactivate.Identity().Username, auditlogUsername)
		// lastly, verify that everything belonging to the user to keep intact remained as-is
		loadedUser = s.Graph.LoadUser(userToStayIntact.IdentityID())
		assert.True(t, loadedUser.User().Active)
		testsupport.AssertIdentityEqual(t, userToStayIntact.Identity(), loadedUser.Identity())
		for _, tID := range []uuid.UUID{token3.TokenID(), token4.TokenID()} {
			tok := s.Graph.LoadToken(tID)
			require.NotNil(t, tok)
			assert.True(t, tok.Token().Valid())
		}
		_, err = s.Application.ExternalTokens().Load(ctx, githubTokenToKeep.ID())
		require.NoError(t, err)
		_, err = s.Application.ExternalTokens().Load(ctx, openshiftTokenToKeep.ID())
		require.NoError(t, err)

	})

	s.T().Run("not found", func(t *testing.T) {
		// when
		_, err := s.Application.UserService().DeactivateUser(s.Ctx, "unknown")
		// then
		testsupport.AssertError(t, err, errors.NotFoundError{}, "user identity with username 'unknown' not found")
	})
}

func (s *userServiceBlackboxTestSuite) TestHardDeleteUser() {

	s.T().Run("ok", func(t *testing.T) {
		user := s.Graph.CreateUser()

		err := s.Application.UserService().HardDeleteUser(s.Ctx, *user.Identity())
		require.NoError(t, err)

		includeSoftDeletes := func(db *gorm.DB) *gorm.DB {
			return db.Unscoped()
		}

		userID := user.User().ID
		loadedUser, err := s.Application.Users().Load(s.Ctx, userID, includeSoftDeletes)
		require.EqualError(s.T(), err, fmt.Sprintf("user with id '%s' not found", userID))
		require.Nil(t, loadedUser)

		loadedUser, err = s.Application.Users().Load(s.Ctx, userID)
		require.EqualError(t, err, fmt.Sprintf("user with id '%s' not found", userID))
		require.Nil(t, loadedUser)

		identityID := user.IdentityID()
		identity, err := s.Application.Identities().Load(s.Ctx, identityID, includeSoftDeletes)
		require.EqualError(t, err, fmt.Sprintf("identity with id '%s' not found", identityID))
		require.Nil(t, identity)

		identity, err = s.Application.Identities().Load(s.Ctx, identityID)
		require.EqualError(t, err, fmt.Sprintf("identity with id '%s' not found", identityID))
		require.Nil(t, identity)
	})
}

func (s *userServiceBlackboxTestSuite) TestResetBan() {

	userToBan := s.Graph.CreateUser()
	userToStayIntact := s.Graph.CreateUser()

	ctx, _, _ := testtoken.ContextWithTokenAndRequestID(s.T())
	ctx = testtoken.ContextWithRequest(ctx)

	// call to Tenant Service
	defer gock.Off()
	gock.New("http://localhost:8090").
		Delete(fmt.Sprintf("/api/tenants/%s", userToBan.IdentityID().String())).
		Reply(204)
	// call to Che
	gock.New("http://localhost:8091").
		Delete(fmt.Sprintf("/api/user/%s", userToBan.IdentityID().String())).
		Reply(204)

	identity, err := s.Application.UserService().BanUser(ctx, userToBan.Identity().Username)
	require.NoError(s.T(), err)
	assert.True(s.T(), identity.User.Banned)

	gock.New("http://localhost:8090").
		Delete(fmt.Sprintf("/api/tenants/%s", userToStayIntact.IdentityID().String())).
		Reply(204)
	// call to Che
	gock.New("http://localhost:8091").
		Delete(fmt.Sprintf("/api/user/%s", userToStayIntact.IdentityID().String())).
		Reply(204)

	identityToStayIntact, err := s.Application.UserService().BanUser(ctx, userToStayIntact.Identity().Username)
	require.NoError(s.T(), err)
	assert.True(s.T(), identityToStayIntact.User.Banned)

	err = s.Application.UserService().ResetBan(s.Ctx, identity.User)
	require.NoError(s.T(), err)

	loadedUser := s.Graph.LoadUser(userToBan.IdentityID())
	assert.False(s.T(), loadedUser.User().Banned)

	loadedUser = s.Graph.LoadUser(userToStayIntact.IdentityID())
	assert.True(s.T(), loadedUser.User().Banned)
}

func (s *userServiceBlackboxTestSuite) TestIdentityByUsernameAndEmail() {
	s.T().Run("found", func(t *testing.T) {
		user := s.Graph.CreateUser()
		s.Graph.CreateUser() // noise

		identity, err := s.Application.UserService().IdentityByUsernameAndEmail(s.Ctx, user.Identity().Username, user.User().Email)
		require.NoError(t, err)
		loadedUser := s.Graph.LoadUser(user.IdentityID())
		testsupport.AssertIdentityEqual(t, identity, loadedUser.Identity())
	})

	s.T().Run("unknown", func(t *testing.T) {
		user := s.Graph.CreateUser()
		s.T().Run("unknown email", func(t *testing.T) {
			identity, err := s.Application.UserService().IdentityByUsernameAndEmail(s.Ctx, user.Identity().Username, uuid.NewV4().String())
			require.NoError(t, err)
			assert.Nil(t, identity)
		})
		s.T().Run("unknown username", func(t *testing.T) {
			identity, err := s.Application.UserService().IdentityByUsernameAndEmail(s.Ctx, uuid.NewV4().String(), user.User().Email)
			require.NoError(t, err)
			assert.Nil(t, identity)
		})
		s.T().Run("unknown username and email", func(t *testing.T) {
			identity, err := s.Application.UserService().IdentityByUsernameAndEmail(s.Ctx, uuid.NewV4().String(), uuid.NewV4().String())
			require.NoError(t, err)
			assert.Nil(t, identity)
		})
	})
}

func (s *userServiceBlackboxTestSuite) TestShowUserInfoOK() {

	s.T().Run("ok", func(t *testing.T) {
		// given a sample user and identity
		identity, ctx, err := testsupport.EmbedTestIdentityTokenInContext(s.DB, "UserServiceBlackBoxTest-User")
		require.Nil(t, err)
		// when
		retrievedUser, retrievedIdentity, err := s.Application.UserService().UserInfo(ctx, identity.ID)
		require.Nil(t, err)
		// then
		assert.Equal(t, identity.User.Email, retrievedUser.Email)
		assert.Equal(t, identity.User.FullName, retrievedUser.FullName)
		assert.Equal(t, identity.Username, retrievedIdentity.Username)
		assert.Equal(t, identity.ID, retrievedIdentity.ID)
		assert.Equal(t, identity.ID, retrievedIdentity.ID)

	})

	s.T().Run("not found", func(t *testing.T) {
		// given a random ID
		id := uuid.NewV4()
		// when
		_, _, err := s.Application.UserService().UserInfo(context.Background(), id)
		// then
		require.Error(t, err)
		assert.IsType(t, errors.UnauthorizedError{}, errs.Cause(err))
	})

}
