package notify

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Branded OTP template contract (M-Login): escaping, brand, code, both parts.
func TestOTPEmail(t *testing.T) {
	email := OtpEmail{Brand: "AmaniConnect", Code: "653495", Minutes: 10, Color: "#145377"}

	text := email.RenderText()
	assert.Contains(t, text, "Your AmaniConnect verification code is 653495")
	assert.Contains(t, text, "Betta Technologies", "footer attribution stays on the platform operator")

	htmlMsg := email.RenderHTML()
	assert.Contains(t, htmlMsg, ">653495<", "code appears")
	assert.Contains(t, htmlMsg, "#145377")
	assert.Contains(t, htmlMsg, "AmaniConnect")
	assert.Contains(t, htmlMsg, "Expires in 10 minutes")
	assert.True(t, strings.Contains(htmlMsg, "<!doctype html>"))

	// XSS-ish injection is always escaped
	xss := OtpEmail{Brand: `<script>alert</script>`, Code: "653495", Minutes: 10}
	out := xss.RenderHTML()
	assert.Contains(t, out, "&lt;script&gt;")
	assert.NotContains(t, out, "<script>")
}
