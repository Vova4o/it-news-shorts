#!/bin/sh
set -eu

psql -v ON_ERROR_STOP=1 \
    --username "$POSTGRES_USER" \
    --dbname "$POSTGRES_DB" \
    -v app_db_user="$APP_DB_USER" \
    -v app_db_password="$APP_DB_PASSWORD" \
    -v app_db_name="$APP_DB_NAME" \
    -v temporal_db_user="$TEMPORAL_DB_USER" \
    -v temporal_db_password="$TEMPORAL_DB_PASSWORD" <<-'EOSQL'
    \o /dev/null
    SELECT set_config('it_news.app_user', :'app_db_user', false);
    SELECT set_config('it_news.app_password', :'app_db_password', false);
    SELECT set_config('it_news.temporal_user', :'temporal_db_user', false);
    SELECT set_config('it_news.temporal_password', :'temporal_db_password', false);
    \o

    DO $$
    DECLARE
        v_app_user      TEXT := current_setting('it_news.app_user');
        v_app_pass      TEXT := current_setting('it_news.app_password');
        v_temp_user     TEXT := current_setting('it_news.temporal_user');
        v_temp_pass     TEXT := current_setting('it_news.temporal_password');
    BEGIN
        IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = v_app_user) THEN
            EXECUTE format('CREATE ROLE %I WITH LOGIN PASSWORD %L', v_app_user, v_app_pass);
        END IF;
        IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = v_temp_user) THEN
            EXECUTE format('CREATE ROLE %I WITH LOGIN PASSWORD %L', v_temp_user, v_temp_pass);
        END IF;
    END
    $$;

    SELECT format('CREATE DATABASE %I OWNER %I', :'app_db_name', :'app_db_user')
    WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'app_db_name')\gexec

    SELECT format('CREATE DATABASE temporal OWNER %I', :'temporal_db_user')
    WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'temporal')\gexec

    SELECT format('CREATE DATABASE temporal_visibility OWNER %I', :'temporal_db_user')
    WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'temporal_visibility')\gexec

    SELECT format('REVOKE ALL ON DATABASE %I FROM PUBLIC', :'app_db_name')\gexec
    SELECT format('GRANT ALL PRIVILEGES ON DATABASE %I TO %I', :'app_db_name', :'app_db_user')\gexec
    SELECT format('REVOKE ALL ON DATABASE temporal FROM PUBLIC')\gexec
    SELECT format('GRANT ALL PRIVILEGES ON DATABASE temporal TO %I', :'temporal_db_user')\gexec
    SELECT format('REVOKE ALL ON DATABASE temporal_visibility FROM PUBLIC')\gexec
    SELECT format('GRANT ALL PRIVILEGES ON DATABASE temporal_visibility TO %I', :'temporal_db_user')\gexec
EOSQL
