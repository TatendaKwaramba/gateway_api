// Package testutil provides helpers for integration and E2E tests.
package testutil

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

// SetupTestDB spins up an ephemeral MySQL container via dockertest, creates the
// required schema, seeds a mock gateway and a tariff plan, and returns the
// database handle plus a cleanup function.
func SetupTestDB() (*sql.DB, func(), error) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		return nil, nil, fmt.Errorf("could not construct pool: %w", err)
	}

	err = pool.Client.Ping()
	if err != nil {
		return nil, nil, fmt.Errorf("could not connect to Docker/Podman: %w", err)
	}

	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "mysql",
		Tag:        "8.0",
		Env: []string{
			"MYSQL_ROOT_PASSWORD=secret",
			"MYSQL_DATABASE=radius",
		},
	}, func(config *docker.HostConfig) {
		config.AutoRemove = true
		config.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("could not start resource: %w", err)
	}

	cleanup := func() {
		if err := pool.Purge(resource); err != nil {
			log.Printf("Could not purge resource: %s", err)
		}
	}

	dsn := fmt.Sprintf("root:secret@tcp(localhost:%s)/radius?parseTime=true&multiStatements=true", resource.GetPort("3306/tcp"))

	// exponential backoff-retry because MySQL might not accept connections yet
	var db *sql.DB
	if err := pool.Retry(func() error {
		db, err = sql.Open("mysql", dsn)
		if err != nil {
			return err
		}
		return db.Ping()
	}); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("could not connect to docker MySQL: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := createSchema(db); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("schema creation failed: %w", err)
	}

	if err := seedFixtures(db); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("fixture seeding failed: %w", err)
	}

	return db, cleanup, nil
}

func createSchema(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS payments_paymentgateway (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    gateway_code VARCHAR(50) NOT NULL UNIQUE,
    description TEXT,
    configuration JSON DEFAULT ('{}'),
    is_active TINYINT(1) DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS payments_paymentmethod (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    gateway_id BIGINT NOT NULL,
    method_code VARCHAR(40) NOT NULL,
    display_name VARCHAR(100) NOT NULL,
    icon_url VARCHAR(500),
    is_active TINYINT(1) DEFAULT 1,
    display_order SMALLINT DEFAULT 0,
    metadata JSON DEFAULT ('{}'),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY unique_gateway_method (gateway_id, method_code)
);

CREATE TABLE IF NOT EXISTS payments_paymenttransaction (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    transaction_id VARCHAR(100) NOT NULL UNIQUE,
    gateway_id BIGINT NOT NULL,
    payment_method_id BIGINT,
    amount DECIMAL(10,2) NOT NULL,
    currency VARCHAR(3) DEFAULT 'ZAR',
    tariff_plan_id BIGINT,
    state VARCHAR(20) DEFAULT 'initiated',
    status VARCHAR(20) DEFAULT 'initiated',
    customer_id BIGINT,
    customer_email VARCHAR(254),
    customer_phone VARCHAR(20),
    external_reference VARCHAR(255),
    gateway_response JSON DEFAULT ('{}'),
    idempotency_key VARCHAR(255) UNIQUE,
    fulfillment_kind VARCHAR(20) DEFAULT 'voucher',
    voucher_pin VARCHAR(16),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at DATETIME,
    last_polled_at DATETIME,
    INDEX idx_gateway_external (gateway_id, external_reference),
    INDEX idx_state_created (state, created_at),
    INDEX idx_idempotency (idempotency_key)
);

CREATE TABLE IF NOT EXISTS payments_paymentwebhooklog (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    gateway_code VARCHAR(50) NOT NULL,
    raw_body BLOB,
    headers JSON DEFAULT ('{}'),
    signature_valid TINYINT(1) DEFAULT 0,
    processed TINYINT(1) DEFAULT 0,
    error TEXT,
    received_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    processed_at DATETIME,
    transaction_id BIGINT,
    external_reference VARCHAR(255),
    INDEX idx_gateway_received (gateway_code, received_at),
    INDEX idx_processed_received (processed, received_at),
    INDEX idx_external_ref (external_reference)
);

CREATE TABLE IF NOT EXISTS payments_idempotency_key (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    `key` VARCHAR(255) NOT NULL UNIQUE,
    request_hash VARCHAR(64) NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_key (`key`)
);

CREATE TABLE IF NOT EXISTS payments_webhook_replay_log (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    event_key VARCHAR(255) NOT NULL,
    event_hash VARCHAR(64) NOT NULL,
    received_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_event (event_key, event_hash)
);

CREATE TABLE IF NOT EXISTS notification_attempts (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    transaction_id BIGINT,
    recipient VARCHAR(255) NOT NULL,
    channel VARCHAR(20) DEFAULT 'sms',
    body TEXT NOT NULL,
    status VARCHAR(20) DEFAULT 'pending',
    provider VARCHAR(50),
    error TEXT,
    retry_count INT UNSIGNED DEFAULT 0,
    attempted_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME,
    next_retry_at DATETIME,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_status_retry (status, retry_count, next_retry_at),
    INDEX idx_transaction_attempted (transaction_id, attempted_at)
);

CREATE TABLE IF NOT EXISTS radcheck (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    username VARCHAR(64) NOT NULL,
    attribute VARCHAR(64) NOT NULL,
    op VARCHAR(2) NOT NULL,
    value VARCHAR(253) NOT NULL
);

CREATE TABLE IF NOT EXISTS services_supportedcurrency (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    code VARCHAR(3) NOT NULL UNIQUE,
    name VARCHAR(100) NOT NULL,
    symbol VARCHAR(8) DEFAULT '',
    minor_units_per_major INT UNSIGNED DEFAULT 100,
    decimal_places SMALLINT UNSIGNED DEFAULT 2,
    is_active TINYINT(1) DEFAULT 1,
    is_default TINYINT(1) DEFAULT 0
);

CREATE TABLE IF NOT EXISTS services_hotspotdaypolicy (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(50) DEFAULT 'default',
    is_active TINYINT(1) DEFAULT 1,
    currency_id BIGINT NOT NULL,
    price_minor_anchor INT UNSIGNED DEFAULT 500,
    seconds_per_day INT UNSIGNED DEFAULT 86400,
    peak_download_kbps INT UNSIGNED DEFAULT 3000,
    peak_upload_kbps INT UNSIGNED DEFAULT 1500,
    fup_mb_per_day INT UNSIGNED DEFAULT 102400,
    fup_throttle_download_kbps INT UNSIGNED DEFAULT 1000,
    fup_throttle_upload_kbps INT UNSIGNED DEFAULT 500,
    max_sessions_base INT UNSIGNED DEFAULT 2,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS services_tariffplan (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    day_policy_id BIGINT,
    service_tier_id BIGINT,
    plan_kind VARCHAR(20) DEFAULT 'hotspot_voucher',
    package_label VARCHAR(50) DEFAULT '',
    marketing_tagline VARCHAR(120) DEFAULT '',
    currency VARCHAR(3) DEFAULT 'ZAR',
    price INT NOT NULL,
    duration_days INT UNSIGNED DEFAULT 1,
    seconds_override INT UNSIGNED,
    time_string VARCHAR(20) NOT NULL,
    seconds INT NOT NULL,
    max_sessions INT NOT NULL DEFAULT 1,
    download_speed INT NOT NULL,
    upload_speed INT NOT NULL,
    download_limit INT,
    upload_limit INT,
    fup_data_quota_mb INT UNSIGNED DEFAULT 0,
    fup_download_speed INT UNSIGNED DEFAULT 1000,
    fup_upload_speed INT UNSIGNED DEFAULT 500,
    fup_quota_window VARCHAR(20) DEFAULT 'plan_period',
    description VARCHAR(100),
    is_active TINYINT(1) DEFAULT 1
);

CREATE TABLE IF NOT EXISTS payments_gatewaysupportedcurrency (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    gateway_id BIGINT NOT NULL,
    currency_id BIGINT NOT NULL,
    is_active TINYINT(1) DEFAULT 1,
    UNIQUE KEY uq_gateway_currency (gateway_id, currency_id)
);

CREATE TABLE IF NOT EXISTS vouchers_voucher (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    radcheck_id BIGINT NOT NULL,
    tariff_plan_id BIGINT,
    voucher_amount INT DEFAULT 0,
    voucher_serial_number VARCHAR(64),
    voucher_pin VARCHAR(64),
    voucher_expired_date DATETIME,
    voucher_response_message TEXT,
    voucher_status TINYINT(1) DEFAULT 1,
    payment_transaction_id BIGINT,
    created_by_id INT NOT NULL DEFAULT 1,
    updated_by_id INT NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);
`
	_, err := db.Exec(schema)
	return err
}

func seedFixtures(db *sql.DB) error {
	// Seed mock gateway
	_, err := db.Exec(`
		INSERT INTO payments_paymentgateway (name, gateway_code, description, configuration, is_active)
		VALUES ('Mock Gateway', 'mock', 'Mock payment gateway for testing', '{"fees": {"percentage": 2.5, "fixed": 0, "currency": "USD"}, "features": ["instant", "no_redirect", "mobile_money", "card"]}', 1)
	`)
	if err != nil {
		return fmt.Errorf("insert mock gateway: %w", err)
	}

	// Seed mock payment methods
	_, err = db.Exec(`
		INSERT INTO payments_paymentmethod (gateway_id, method_code, display_name, display_order, metadata, is_active)
		VALUES 
		(1, 'mock-instant', 'Mock Instant', 10, '{"requires_phone": false, "requires_redirect": false}', 1),
		(1, 'mock-ecocash', 'Mock EcoCash', 20, '{"requires_phone": true, "requires_redirect": false}', 1),
		(1, 'mock-card-redirect', 'Mock Card (Redirect)', 30, '{"requires_phone": false, "requires_redirect": true}', 1),
		(1, 'mock-slow', 'Mock Slow', 40, '{"requires_phone": true, "requires_redirect": false}', 1),
		(1, 'mock-flaky', 'Mock Flaky', 50, '{"requires_phone": true, "requires_redirect": false}', 1)
	`)
	if err != nil {
		return fmt.Errorf("insert mock methods: %w", err)
	}

	_, err = db.Exec(`
		INSERT INTO services_supportedcurrency (code, name, symbol, is_active, is_default)
		VALUES ('ZAR', 'South African Rand', 'R', 1, 1)
	`)
	if err != nil {
		return fmt.Errorf("insert currency: %w", err)
	}

	_, err = db.Exec(`
		INSERT INTO services_hotspotdaypolicy (name, currency_id, is_active)
		VALUES ('default', 1, 1)
	`)
	if err != nil {
		return fmt.Errorf("insert day policy: %w", err)
	}

	_, err = db.Exec(`
		INSERT INTO payments_gatewaysupportedcurrency (gateway_id, currency_id, is_active)
		VALUES (1, 1, 1)
	`)
	if err != nil {
		return fmt.Errorf("insert gateway currency: %w", err)
	}

	_, err = db.Exec(`
		INSERT INTO services_tariffplan (
			day_policy_id, package_label, currency, price, duration_days,
			time_string, seconds, download_speed, upload_speed, max_sessions,
			fup_data_quota_mb, fup_download_speed, fup_upload_speed, is_active
		) VALUES (1, 'Day Pass', 'ZAR', 500, 1, '24:00:00', 86400, 3000, 1500, 2, 102400, 1000, 500, 1)
	`)
	if err != nil {
		return fmt.Errorf("insert tariff plan: %w", err)
	}

	return nil
}
