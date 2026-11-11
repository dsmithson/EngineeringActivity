--liquibase formatted sql

--changeset dashboard:001-app-metadata
CREATE TABLE app_metadata (
    key text PRIMARY KEY,
    value text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
