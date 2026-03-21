-- Forge Pool Database Schema
-- Auto-created on first run

-- Miners table
CREATE TABLE IF NOT EXISTS miners (
    id SERIAL PRIMARY KEY,
    address VARCHAR(255) NOT NULL UNIQUE,
    hashrate NUMERIC(20,2) DEFAULT 0,
    shares_count BIGINT DEFAULT 0,
    balance NUMERIC(20,8) DEFAULT 0,
    total_paid NUMERIC(20,8) DEFAULT 0,
    last_share TIMESTAMP,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW(),
    solo_mining BOOLEAN DEFAULT FALSE,
    manual_diff NUMERIC(20,8) DEFAULT 0,
    min_payout NUMERIC(20,8) DEFAULT 5.0
);
CREATE INDEX IF NOT EXISTS idx_miners_address ON miners(address);

-- Blocks table
CREATE TABLE IF NOT EXISTS blocks (
    id SERIAL PRIMARY KEY,
    height INTEGER NOT NULL,
    hash VARCHAR(64) NOT NULL UNIQUE,
    miner_address VARCHAR(255),
    worker_name VARCHAR(255),
    reward NUMERIC(20,8) DEFAULT 50,
    difficulty NUMERIC(30,8),
    confirmations INTEGER DEFAULT 0,
    status VARCHAR(20) DEFAULT 'pending',
    solo BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT NOW(),
    confirmed_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_blocks_height ON blocks(height);
CREATE INDEX IF NOT EXISTS idx_blocks_miner ON blocks(miner_address);
CREATE INDEX IF NOT EXISTS idx_blocks_status ON blocks(status);

-- Shares table (for stats, can use TimescaleDB hypertable)
CREATE TABLE IF NOT EXISTS shares (
    id BIGSERIAL PRIMARY KEY,
    miner_address VARCHAR(255) NOT NULL,
    worker_name VARCHAR(255),
    difficulty NUMERIC(20,8) NOT NULL,
    share_diff NUMERIC(20,8) NOT NULL,
    valid BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_shares_miner ON shares(miner_address);
CREATE INDEX IF NOT EXISTS idx_shares_time ON shares(created_at DESC);

-- Payouts table
CREATE TABLE IF NOT EXISTS payouts (
    id SERIAL PRIMARY KEY,
    miner_address VARCHAR(255) NOT NULL,
    amount NUMERIC(20,8) NOT NULL,
    txid VARCHAR(64),
    status VARCHAR(20) DEFAULT 'pending',
    created_at TIMESTAMP DEFAULT NOW(),
    confirmed_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_payouts_miner ON payouts(miner_address);
CREATE INDEX IF NOT EXISTS idx_payouts_status ON payouts(status);

-- Pool stats table
CREATE TABLE IF NOT EXISTS pool_stats (
    id SERIAL PRIMARY KEY,
    hashrate NUMERIC(30,2) DEFAULT 0,
    miners_count INTEGER DEFAULT 0,
    workers_count INTEGER DEFAULT 0,
    blocks_found INTEGER DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW()
);

-- Miner settings (for vardiff, solo mode, etc.)
CREATE TABLE IF NOT EXISTS miner_settings (
    id SERIAL PRIMARY KEY,
    address VARCHAR(255) NOT NULL UNIQUE,
    solo_mining BOOLEAN DEFAULT FALSE,
    manual_diff NUMERIC(20,8) DEFAULT 0,
    vardiff BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_miner_settings_address ON miner_settings(address);

-- Convert shares to TimescaleDB hypertable if available
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM create_hypertable('shares', 'created_at', if_not_exists => TRUE);
    END IF;
EXCEPTION WHEN OTHERS THEN
    NULL;
END $$;
