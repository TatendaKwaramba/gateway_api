package mailgun

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freeradius/payments-api/internal/notify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendPostsToMailgunAPI(t *testing.T) {
	var gotPath, gotAuth, gotForm string
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotForm = string(b)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"Queued. Thank you.","id":"<id@domain>"}`))
	}))
	defer server.Close()

	provider := NewProvider(
		"test-api-key",
		"sandbox9e6af233cc634281b5f9a7ab0285b8e0.mailgun.org",
		"AmaniConnect <postmaster@sandbox9e6af233cc634281b5f9a7ab0285b8e0.mailgun.org>",
		server.URL,
	)
	err := provider.Send(context.Background(), notify.Message{
		To:      "kwarambaandy@gmail.com",
		Subject: "Your verification code",
		Body:    "Code 123456",
	})
	require.NoError(t, err)

	assert.Equal(t, "/v3/sandbox9e6af233cc634281b5f9a7ab0285b8e0.mailgun.org/messages", gotPath)
	assert.True(t, strings.HasPrefix(gotAuth, "Basic "))
	assert.Contains(t, gotForm, "to=kwarambaandy%40gmail.com")
	assert.Contains(t, gotForm, "subject=")
	assert.Contains(t, gotForm, "Code+123456") // url-encoded body contains the code
	assert.Contains(t, gotForm, "text=")
}

func TestSendSMSError(t *testing.T) {
	provider := NewProvider("k", "d", "f", "")
	err := provider.SendSMS(context.Background(), "x", "y")
	assert.ErrorContains(t, err, "SMS not supported")
}

func TestRejectsMissingRecipient(t *testing.T) {
	provider := NewProvider("k", "d", "f", "")
	assert.ErrorContains(t, provider.Send(context.Background(), notify.Message{Body: "hi"}), "recipient")
}
