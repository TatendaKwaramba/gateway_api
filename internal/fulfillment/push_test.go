package fulfillment

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/freeradius/payments-api/internal/identity"
	"github.com/freeradius/payments-api/internal/notify"
	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingPushSender implements notify.PushSender for M6 tests.
type recordingPushSender struct {
	pushes []string
	err    error
}

func (r *recordingPushSender) Name() string { return "mock-push" }

func (r *recordingPushSender) SendPush(_ context.Context, to, title, _ string, _ map[string]string) error {
	r.pushes = append(r.pushes, to)
	return r.err
}

// TestPushNotification exercises the FUP-threshold push primitive (M6).
func TestPushNotification(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}

	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	secret := "identity-hmac-test-secret-32-characters-ok"
	notifier := notifymock.NewMockProvider()
	svc := NewService(db, notifier)
	push := &recordingPushSender{}
	identitySvc := identity.NewService(db, notifier, nil, secret, "")
	svc.SetPushSender(push, identitySvc)

	ctx := context.Background()
	seedDevice := func(phone, token string) {
		t.Helper()
		_, err := db.Exec(`
			INSERT INTO payments_pushdevice (identity_phone, expo_token, platform, enabled)
			VALUES (?, ?, 'android', TRUE)
		`, phone, token)
		require.NoError(t, err)
	}

	t.Run("no devices → friendly error", func(t *testing.T) {
		err := svc.PushNotification(ctx, PushNotificationRequest{
			Phone: "+15550001234", Title: "T", Body: "B",
			Data: map[string]string{"kind": "fup"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no registered push devices")
	})

	t.Run("validation", func(t *testing.T) {
		err := svc.PushNotification(ctx, PushNotificationRequest{Phone: "", Title: "T", Body: "B"})
		require.ErrorContains(t, err, "required")
	})

	t.Run("delivers to all enabled devices matching phone formats", func(t *testing.T) {
		seedDevice("0771234567", "Expo-e7-token-1")
		seedDevice("+263771234567", "Expo-e7-token-2")
		err := svc.PushNotification(ctx, PushNotificationRequest{
			Phone: "0771234567", Title: "Fair-use warning", Body: "You hit 80%.",
			Data: map[string]string{"kind": "fup", "pct": "80", "screen": "usage"},
		})
		require.NoError(t, err)
		require.Len(t, push.pushes, 2)
	})

	t.Run("DeviceNotRegistered disables the token", func(t *testing.T) {
		seedDevice("+263771234567", "Expo-e7-dead-token")
		push.err = fmt.Errorf("expo: DeviceNotRegistered: %w", notify.ErrDeviceNotRegistered)
		defer func() { push.err = nil }()

		err := svc.PushNotification(ctx, PushNotificationRequest{
			Phone: "+263771234567", Title: "T", Body: "B",
		})
		require.Error(t, err)

		var enabled bool
		require.NoError(t, db.QueryRow(`
			SELECT enabled FROM payments_pushdevice WHERE expo_token = 'Expo-e7-dead-token'
		`).Scan(&enabled))
		assert.False(t, enabled, "dead tokens are disabled like sendPushForPhone does")
	})

	t.Run("audited in notification_attempts with transaction_id=0", func(t *testing.T) {
		seedDevice("+263771234567", "Expo-e7-audit-token")
		err := svc.PushNotification(ctx, PushNotificationRequest{
			Phone: "+263771234567", Title: "Daily quota", Body: "Reached 100%.",
		})
		require.NoError(t, err)
		require.NoError(t, db.QueryRow(`SELECT 1`).Scan(new(int)))

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			checkErr := db.QueryRow(`
				SELECT COUNT(*) FROM notification_attempts
				WHERE channel = 'push' AND body = 'Reached 100%.'
			`).Scan(&n)
			require.NoError(t, checkErr)
			if n > 0 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("no notification_attempts row written for the push")
	})
}
