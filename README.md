# Payments API

Go service for payment gateway processing.

**Status:** Phase 1a - Skeleton implementation with Mock Gateway

See `docs/PAYMENT_GATEWAY_MODULAR_PLAN.md` for full specification.

## Quick Start

```bash
# Build
go build -o bin/server cmd/server/main.go

# Run with mock gateway
MOCK_GATEWAY_ENABLED=true \
MOCK_GATEWAY_WEBHOOK_SECRET=dev-secret-min-32-characters-long \
ADMIN_HMAC_SECRET=admin-secret-min-32-characters-long \
DB_DSN="user:pass@tcp(localhost:3306)/radius?parseTime=true" \
go run cmd/server/main.go

# Run tests
go test ./...
```

## Environment Variables

### Required

- `DB_DSN` - MySQL connection string (e.g., `user:pass@tcp(localhost:3306)/radius?parseTime=true`)
- `ADMIN_HMAC_SECRET` - Shared secret for admin authentication (min 32 chars)

### Optional

- `APP_ENV` - Application environment (`development` or `production`, default: `development`)
- `PORT` - HTTP server port (default: `8080`)
- `LOG_LEVEL` - Log level (`debug`, `info`, `warn`, `error`, default: `info`)

### Currency

- `DEFAULT_CURRENCY` - Default currency for tariff plans (default: `USD`)

### Mock Gateway (Development Only)

- `MOCK_GATEWAY_ENABLED` - Enable mock gateway (`true` or `false`)
- `MOCK_GATEWAY_WEBHOOK_SECRET` - Secret for mock webhook signing (required if enabled)
- `MOCK_RETURN_URL` - Return URL for redirect flows

### Paynow Gateway (Phase 1b)

- `PAYNOW_INTEGRATION_ID` - Paynow integration ID
- `PAYNOW_INTEGRATION_KEY` - Paynow integration key
- `PAYNOW_RESULT_URL` - Webhook URL for Paynow callbacks
- `PAYNOW_RETURN_URL` - Customer return URL

### Notifications

- `NOTIFY_PROVIDER` - Notification provider (`mock`, `twilio`, or `africastalking`)
- `TWILIO_ACCOUNT_SID` - Twilio account SID
- `TWILIO_AUTH_TOKEN` - Twilio auth token
- `TWILIO_PHONE_NUMBER` - Twilio phone number
- `AT_USERNAME` - Africa's Talking username
- `AT_API_KEY` - Africa's Talking API key

## API Endpoints

### Public Endpoints

- `GET /health` - Health check
- `GET /api/payments/methods?currency=USD` - List available payment methods
- `POST /api/payments/initiate` - Initiate a payment
- `GET /api/payments/{transaction_id}/status` - Get payment status

### Webhooks

- `POST /webhooks/{gateway_code}` - Receive webhooks from payment gateways

### Admin Endpoints (Authenticated)

- `GET /admin/api/payments/{transaction_id}` - Get transaction details
- `POST /admin/api/payments/{transaction_id}/refund` - Process refund
- `POST /admin/api/payments/{transaction_id}/cancel` - Cancel transaction
- `GET /admin/api/gateways` - List configured gateways
- `GET /admin/api/gateways/{gateway_code}/schema` - Get gateway config schema

### Mock Gateway Endpoints (Development Only)

- `GET /api/mock/transactions` - List mock transactions
- `POST /api/mock/transactions/{external_ref}/complete` - Manually complete a transaction
- `POST /api/mock/transactions/{external_ref}/fail` - Manually fail a transaction
- `POST /api/mock/transactions/{external_ref}/refund` - Manually refund a transaction
- `POST /api/mock/transactions/{external_ref}/webhook` - Trigger webhook
- `GET /mock/checkout/{external_ref}` - Mock checkout page

## Mock Gateway Magic Values

Use these phone suffixes to trigger specific behaviors during testing:

| Suffix | Behavior |
|--------|----------|
| `0001` | Instant success |
| `0002` | Async success after 3 seconds |
| `0003` | Fail - insufficient funds |
| `0004` | Fail - customer declined |
| `0005` | Pending forever (poller test) |
| `0006` | Webhook with invalid signature |
| `0007` | Webhook replay test |
| `0008` | Network error on initiate |
| `0009` | Slow initiate (~10s) |

Amount triggers:
- Ends with `.13` - Chargeback simulation (auto-refund after 10s)
- Ends with `.99` - Fulfillment failure simulation

## Project Structure

```
payments_api/
├── cmd/server/main.go          # HTTP server entrypoint
├── internal/
│   ├── config/                 # Environment configuration
│   ├── db/                     # Database queries (sqlc)
│   ├── httpapi/                # HTTP handlers and routing
│   ├── gateways/               # Gateway adapters
│   │   ├── registry.go         # Gateway interface & registry
│   │   └── mock/               # Mock gateway implementation
│   └── payments/               # Core payment logic
│       ├── statemachine.go     # Payment state management
│       ├── idempotency.go      # Idempotency handling
│       └── service.go          # Payment service
├── go.mod
├── go.sum
├── Dockerfile
└── README.md
```

## Development

### Prerequisites

- Go 1.21+
- MySQL 8.0+
- Docker (optional)

### Running Locally

1. Start MySQL (using Docker):
```bash
docker run -d --name mysql \
  -e MYSQL_ROOT_PASSWORD=root \
  -e MYSQL_DATABASE=radius \
  -p 3306:3306 mysql:8.0
```

2. Run migrations (from flash-api):
```bash
cd ../microtik_flash_api
python manage.py migrate payments
```

3. Start the server:
```bash
go run cmd/server/main.go
```

### Testing

```bash
# Run all tests
go test ./...

# Run with coverage
go test -cover ./...

# Run specific package
go test ./internal/payments/...
```

### Example API Calls

```bash
# List payment methods
curl http://localhost:8080/api/payments/methods

# Initiate a payment
curl -X POST http://localhost:8080/api/payments/initiate \
  -H "Content-Type: application/json" \
  -d '{
    "gateway_code": "mock",
    "method_code": "mock-instant",
    "amount": 500,
    "currency": "USD",
    "customer_phone": "0772000001"
  }'

# Check status
curl http://localhost:8080/api/payments/1/status
```

## License

Part of the FreeRADIUS ISP Platform.
