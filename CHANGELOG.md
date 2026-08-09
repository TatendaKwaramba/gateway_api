# Changelog

All notable changes to this project will be documented in this file.

## [0.1.0] - 2026-08-09

### Added

- Initial release
- Go service with Chi router
- Gateway adapter pattern (mock, Paynow, EcoCash)
- Payment state machine with atomic DB transitions
- Idempotency key support
- Webhook handling with replay detection
- Voucher fulfillment (16-digit PIN, RADIUS attributes)
- Subscription fulfillment (subscriber password, VLAN, Framed-Pool)
- Notification providers (Twilio, Africa's Talking, Mock)
- Background workers (poller, notification retry, reconciler)
- Prometheus metrics and Grafana dashboard
- Admin HMAC authentication middleware
- Health endpoint and readiness probe
