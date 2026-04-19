-- Forge Pool Database Schema for Umbrel
-- Version 1.3.2 - Security and performance fixes

-- Miners table
CREATE TABLE IF NOT EXISTS miners (
    id SERIAL PRIMARY KEY,
    address VARCHAR(255) NOT NULL UNIQUE,
    solo_mining BOOLEAN DEFAULT FALSE,
    manual_diff NUMERIC(20,8) DEFAULT 0,
    min_payout NUMERIC(20,8) DEFAULT 5.0,
    balance NUMERIC(20,8) DEFAULT 0,
    total_paid NUMERIC(20,8) DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_miners_address ON miners(address);

-- Blocks table with status column for confirmation tracking
CREATE TABLE IF NOT EXISTS blocks (
    id SERIAL PRIMARY KEY,
    height INTEGER NOT NULL,
    hash VARCHAR(64) NOT NULL UNIQUE,
    miner_address VARCHAR(255),
    worker_name VARCHAR(255),
    reward NUMERIC(20,8) DEFAULT 50,
    status VARCHAR(20) DEFAULT 'pending',
    is_solo BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_blocks_height ON blocks(height);
CREATE INDEX IF NOT EXISTS idx_blocks_miner ON blocks(miner_address);
CREATE INDEX IF NOT EXISTS idx_blocks_status ON blocks(status);
CREATE INDEX IF NOT EXISTS idx_blocks_created ON blocks(created_at DESC);
-- Unique constraint on height to prevent duplicate blocks
CREATE UNIQUE INDEX IF NOT EXISTS idx_blocks_height_unique ON blocks(height);

-- Shares table with optimized indexes for PPLNS queries
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
-- Index for PPLNS window queries (get recent non-solo shares)
CREATE INDEX IF NOT EXISTS idx_shares_pplns ON shares(id DESC) WHERE is_solo = FALSE;
-- Composite index for efficient share lookup
CREATE INDEX IF NOT EXISTS idx_shares_miner_time ON shares(miner_address, created_at DESC);

-- Payouts table with status tracking
CREATE TABLE IF NOT EXISTS payouts (
    id SERIAL PRIMARY KEY,
    miner_address VARCHAR(255) NOT NULL,
    block_height INTEGER,
    amount NUMERIC(20,8) NOT NULL,
    txid VARCHAR(64),
    confirmed BOOLEAN DEFAULT FALSE,
    status VARCHAR(20) DEFAULT 'pending',
    is_solo BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT NOW(),
    paid_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_payouts_miner ON payouts(miner_address);
CREATE INDEX IF NOT EXISTS idx_payouts_confirmed ON payouts(confirmed);
CREATE INDEX IF NOT EXISTS idx_payouts_txid ON payouts(txid) WHERE txid IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_payouts_status ON payouts(status);
-- Index for finding unpaid mature payouts efficiently
CREATE INDEX IF NOT EXISTS idx_payouts_unpaid ON payouts(miner_address, block_height) WHERE txid IS NULL OR txid = '';
-- Index for block height lookups
CREATE INDEX IF NOT EXISTS idx_payouts_height ON payouts(block_height);

-- Migrations for existing databases
DO $$ BEGIN
    -- Add paid_at column if missing
    ALTER TABLE payouts ADD COLUMN IF NOT EXISTS paid_at TIMESTAMP;
EXCEPTION WHEN others THEN NULL;
END $$;

DO $$ BEGIN
    -- Add share_diff column if missing
    ALTER TABLE shares ADD COLUMN IF NOT EXISTS share_diff NUMERIC(30,8) DEFAULT 0;
EXCEPTION WHEN others THEN NULL;
END $$;

DO $$ BEGIN
    -- Add status column to blocks if missing
    ALTER TABLE blocks ADD COLUMN IF NOT EXISTS status VARCHAR(20) DEFAULT 'pending';
EXCEPTION WHEN others THEN NULL;
END $$;

DO $$ BEGIN
    -- Add status column to payouts if missing
    ALTER TABLE payouts ADD COLUMN IF NOT EXISTS status VARCHAR(20) DEFAULT 'pending';
EXCEPTION WHEN others THEN NULL;
END $$;

DO $$ BEGIN
    -- Add balance and total_paid columns to miners if missing
    ALTER TABLE miners ADD COLUMN IF NOT EXISTS balance NUMERIC(20,8) DEFAULT 0;
    ALTER TABLE miners ADD COLUMN IF NOT EXISTS total_paid NUMERIC(20,8) DEFAULT 0;
EXCEPTION WHEN others THEN NULL;
END $$;
