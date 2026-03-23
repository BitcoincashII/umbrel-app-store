#!/bin/bash
# This script runs on postgres startup to ensure password is correct
# It modifies pg_hba.conf to allow trust auth, starts postgres, fixes password, then restarts with normal auth

PGDATA=${PGDATA:-/var/lib/postgresql/data}

# Only run fix if database already exists (upgrade scenario)
if [ -f "$PGDATA/PG_VERSION" ]; then
    echo "Existing database detected, ensuring password is correct..."
    
    # Backup original pg_hba.conf
    cp "$PGDATA/pg_hba.conf" "$PGDATA/pg_hba.conf.bak"
    
    # Allow trust auth temporarily
    echo "local all all trust" > "$PGDATA/pg_hba.conf"
    echo "host all all 127.0.0.1/32 trust" >> "$PGDATA/pg_hba.conf"
    echo "host all all ::1/128 trust" >> "$PGDATA/pg_hba.conf"
    
    # Start postgres in background
    pg_ctl -D "$PGDATA" -o "-c listen_addresses=''" -w start
    
    # Fix the password
    psql -U forge -d forgepool -c "ALTER USER forge WITH PASSWORD 'moneyprintergobrrr';" 2>/dev/null || \
    psql -U postgres -d postgres -c "ALTER USER forge WITH PASSWORD 'moneyprintergobrrr';" 2>/dev/null || true
    
    # Stop postgres
    pg_ctl -D "$PGDATA" -w stop
    
    # Restore original pg_hba.conf
    mv "$PGDATA/pg_hba.conf.bak" "$PGDATA/pg_hba.conf"
    
    echo "Password fix complete."
fi
