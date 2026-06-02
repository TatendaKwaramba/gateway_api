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
    currency VARCHAR(3) DEFAULT 'USD',
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

CREATE TABLE IF NOT EXISTS authentication_tariffplan (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    description VARCHAR(255),
    price DECIMAL(10,2) NOT NULL,
    seconds INT NOT NULL,
    download_speed INT NOT NULL,
    upload_speed INT NOT NULL,
    max_sessions INT NOT NULL DEFAULT 1,
    is_active TINYINT(1) DEFAULT 1
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

	// Seed a tariff plan ($5 = 500 cents, price 5.00)
	_, err = db.Exec(`
		INSERT INTO authentication_tariffplan (description, price, seconds, download_speed, upload_speed, max_sessions, is_active)
		VALUES ('5 USD Plan', 5.00, 3600, 10, 5, 1, 1)
	`)
	if err != nil {
		return fmt.Errorf("insert tariff plan: %w", err)
	}

	return nil
}
