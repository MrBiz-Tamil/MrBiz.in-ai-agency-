-- Migration: 000004_agent_spend.down.sql
ALTER TABLE agent_definitions DROP COLUMN IF EXISTS spent_on;
