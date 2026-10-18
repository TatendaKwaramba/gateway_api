// Package notify — branded HTML email template for identity codes (M-Login).
//
// Escapes every dynamic value; the layout mirrors the mobile app's visual
// language (brand color header, big mono code, expiry + do-not-share
// rules, "Powered by" attribution). A plain-text fallback is always built —
// clients that block HTML (or strict DMARC/anti-spoof filters) still show
// a readable code.
package notify

import (
	"fmt"
	"html"
	"strings"
)

// otpEmail is the payload the identity service renders into both body parts.
type OtpEmail struct {
	Brand   string
	Code    string
	Minutes int
	Color   string // accent color (hex no '#'); empty → default brand blue
}

// renderOTPText is the plain fallback (short + actionable).
func (e OtpEmail) RenderText() string {
	return fmt.Sprintf(
		"Your %s verification code is %s. It expires in %d minutes.\n\n"+
			"Do not share this code. If you didn't request it, ignore this email.\n\n"+
			"— %s",
		e.brandName(), e.Code, e.Minutes, parentBrand(e.Brand),
	)
}

// renderHTML is the branded body. Inlined styles only — Gmail strips
// <style> blocks and refuses external CSS.
func (e OtpEmail) RenderHTML() string {
	brand := e.brandName()
	color := e.Color
	if color == "" || !strings.HasPrefix(color, "#") {
		color = "#145377"
	}
	code := html.EscapeString(e.Code)
	name := html.EscapeString(brand)
	power := html.EscapeString(parentBrand(e.Brand))
	return fmt.Sprintf(`<!doctype html>
<html><body style="margin:0;background:#e2e8f0;padding:24px 0;font-family:Arial,Helvetica,sans-serif">
  <table width="100%%" cellpadding="0" cellspacing="0" style="border-collapse:collapse">
    <tr>
      <td align="center" style="padding:0 12px">
        <table width="560" cellpadding="0" cellspacing="0" style="border-collapse:collapse;background:#ffffff;border-radius:16px;overflow:hidden">
          <!-- header -->
          <tr>
            <td style="background:%s;padding:20px 24px;text-align:center">
              <div style="color:#ffffff;font-size:20px;font-weight:bold;letter-spacing:.5px">%s</div>
              <div style="color:#e2e8f0;font-size:12px;margin-top:4px">Verify it's really you</div>
            </td>
          </tr>
          <!-- body -->
          <tr>
            <td style="padding:32px 36px 8px 36px;font-family:Arial,sans-serif;text-align:center">
              <div style="font-size:15px;color:#0f172a;margin-bottom:18px">
                Your %s verification code:
              </div>
              <div style="font-size:36px;letter-spacing:8px;font-weight:bold;color:%s;font-family:Courier New,monospace">%s</div>
              <div style="font-size:13px;color:#64748b;margin-top:16px">
                Expires in 10 minutes · one-time use · don't share this code
              </div>
            </td>
          </tr>
          <!-- security note -->
          <tr>
            <td style="padding:20px 36px 8px 36px">
              <div style="border-top:1px solid #e2e8f0;padding-top:16px;font-size:12px;color:#94a3b8;line-height:1.6">
                Didn't request this code? Ignore this email and
                <span style="color:#0f172a">contact our support line</span> — someone
                may be trying to use your number.
              </div>
            </td>
          </tr>
          <!-- footer -->
          <tr>
            <td style="padding:24px 36px 32px 36px;text-align:center;font-size:11px;color:#cbd5e1">
              Powered by %s
            </td>
          </tr>
        </table>
      </td>
    </tr>
  </table>
</body></html>`,
		color, name, name, color, code, power,
	)
}

func (e OtpEmail) brandName() string {
	if e.Brand == "" {
		return "AmaniConnect"
	}
	return e.Brand
}

func parentBrand(name string) string {
	// Keep the footer tagline anchored on the platform operator (Betta Technologies).
	return "Betta Technologies"
}
