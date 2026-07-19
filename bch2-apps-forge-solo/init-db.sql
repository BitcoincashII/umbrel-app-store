-- Forge Solo — core schema, applied on fresh Postgres init.
-- The Go services (internal/stats/db.go InitDB) also create this idempotently on
-- every start, so this file is a belt-and-suspenders guarantee that a brand-new
-- install has the full schema before any service connects. Kept in sync with
-- internal/stats/db.go corePostgresSchema and database/schema.sql.

DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS timescaledb;
EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'TimescaleDB not available, using standard PostgreSQL';
END $$;

CREATE TABLE IF NOT EXISTS blocks (
    id BIGSERIAL PRIMARY KEY,
    height BIGINT NOT NULL UNIQUE,
    hash VARCHAR(64) NOT NULL UNIQUE,
    miner_address VARCHAR(255) NOT NULL,
    reward DECIMAL(20, 8) NOT NULL DEFAULT 50.0,
    difficulty DECIMAL(30, 8) DEFAULT 0,
    status VARCHAR(20) DEFAULT 'confirmed',
    confirmations INT DEFAULT 0,
    is_solo BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    confirmed_at TIMESTAMP WITH TIME ZONE
);

CREATE TABLE IF NOT EXISTS payouts (
    id BIGSERIAL PRIMARY KEY,
    miner_address VARCHAR(255) NOT NULL,
    block_height BIGINT NOT NULL,
    amount DECIMAL(20, 8) NOT NULL,
    confirmed BOOLEAN DEFAULT FALSE,
    txid VARCHAR(128),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    paid_at TIMESTAMP WITH TIME ZONE,
    UNIQUE(miner_address, block_height)
);

CREATE TABLE IF NOT EXISTS miners (
    id BIGSERIAL PRIMARY KEY,
    address VARCHAR(255) UNIQUE NOT NULL,
    solo_mining BOOLEAN DEFAULT FALSE,
    manual_diff DECIMAL(20, 8) DEFAULT 0,
    min_payout DECIMAL(20, 8) DEFAULT 5.0,
    address_1175 TEXT,
    settings_pin_hash TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS shares (
    id BIGSERIAL,
    time TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    miner_address VARCHAR(255) NOT NULL,
    worker_name VARCHAR(255) NOT NULL,
    job_id VARCHAR(64),
    difficulty DECIMAL(20, 8) NOT NULL,
    is_valid BOOLEAN NOT NULL DEFAULT TRUE,
    is_block BOOLEAN DEFAULT FALSE,
    is_solo BOOLEAN DEFAULT FALSE,
    block_hash VARCHAR(64),
    PRIMARY KEY (id, time)
);

DO $$
BEGIN
    PERFORM create_hypertable('shares', 'time', chunk_time_interval => INTERVAL '1 hour', if_not_exists => TRUE);
EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'Could not create hypertable for shares';
END $$;

CREATE TABLE IF NOT EXISTS pool_stats (
    time TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW() PRIMARY KEY,
    hashrate DECIMAL(30, 2) NOT NULL DEFAULT 0,
    workers INT NOT NULL DEFAULT 0,
    miners_online INT NOT NULL DEFAULT 0,
    valid_shares BIGINT NOT NULL DEFAULT 0,
    invalid_shares BIGINT NOT NULL DEFAULT 0,
    network_difficulty DECIMAL(30, 8) DEFAULT 0,
    block_height BIGINT DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_blocks_height ON blocks(height);
CREATE INDEX IF NOT EXISTS idx_blocks_miner ON blocks(miner_address);
CREATE INDEX IF NOT EXISTS idx_payouts_miner ON payouts(miner_address);
CREATE INDEX IF NOT EXISTS idx_payouts_unpaid ON payouts(miner_address) WHERE txid IS NULL OR txid = '';
CREATE INDEX IF NOT EXISTS idx_payouts_block ON payouts(block_height);
CREATE INDEX IF NOT EXISTS idx_shares_miner_time ON shares(miner_address, time DESC);
CREATE INDEX IF NOT EXISTS idx_miners_address ON miners(address);

SELECT 'forge-solo-miner schema initialized' AS status;
