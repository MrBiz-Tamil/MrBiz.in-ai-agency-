-- Migration: 000004_agent_spend.up.sql
-- Daily spend per agent. The authoritative running total lives in Redis; the kernel mirrors it
-- here so the UI can show it. spent_on records which UTC day the value belongs to, so a value
-- from yesterday is never shown as today's spend.
ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS spent_on DATE NOT NULL DEFAULT (now() AT TIME ZONE 'utc')::date;
