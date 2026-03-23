-- Forge Pool Database Schema for Umbrel

-- Miners table
CREATE TABLE IF NOT EXISTS miners (
    id SERIAL PRIMARY KEY,
    address VARCHAR(255) NOT NULL UNIQUE,
    solo_mining BOOLEAN DEFAULT FALSE,
    manual_diff NUMERIC(20,8) DEFAULT 0,
    min_payout NUMERIC(20,8) DEFAULT 5.0,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
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
    is_solo BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_blocks_height ON blocks(height);
CREATE INDEX IF NOT EXISTS idx_blocks_miner ON blocks(miner_address);

-- Shares table
CREATE TABLE IF NOT EXISTS shares (
    id BIGSERIAL PRIMARY KEY,
    miner_address VARCHAR(255) NOT NULL,
    worker_name VARCHAR(255),
    difficulty NUMERIC(20,8) NOT NULL,
    share_diff NUMERIC(30,8) DEFAULT 0,
    is_solo BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_shares_miner ON shares(miner_address);
CREATE INDEX IF NOT EXISTS idx_shares_time ON shares(created_at DESC);

-- Payouts table
CREATE TABLE IF NOT EXISTS payouts (
    id SERIAL PRIMARY KEY,
    miner_address VARCHAR(255) NOT NULL,
    block_height INTEGER,
    amount NUMERIC(20,8) NOT NULL,
    txid VARCHAR(64),
    confirmed BOOLEAN DEFAULT FALSE,
    is_solo BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT NOW(),
    paid_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_payouts_miner ON payouts(miner_address);
CREATE INDEX IF NOT EXISTS idx_payouts_confirmed ON payouts(confirmed);

-- Migration for existing databases: add paid_at if missing
DO $$ BEGIN
    ALTER TABLE payouts ADD COLUMN IF NOT EXISTS paid_at TIMESTAMP;
EXCEPTION WHEN others THEN NULL;
END $$;

-- Migration for existing databases: add share_diff column
DO $$ BEGIN
    ALTER TABLE shares ADD COLUMN IF NOT EXISTS share_diff NUMERIC(30,8) DEFAULT 0;
EXCEPTION WHEN others THEN NULL;
END $$;
