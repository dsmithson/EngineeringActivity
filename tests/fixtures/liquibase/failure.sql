--liquibase formatted sql
--changeset acceptance:deliberate-failure
SELECT * FROM deliberately_missing_acceptance_table;
