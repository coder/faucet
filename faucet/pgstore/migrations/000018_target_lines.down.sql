DROP TRIGGER faucet_subscriptions_default_position ON faucet_subscriptions;
DROP FUNCTION faucet_subscriptions_default_position();
ALTER TABLE faucet_subscriptions DROP COLUMN position;
DROP FUNCTION faucet_line_position(text, bigint, bigint, bigint);

DROP TRIGGER faucet_events_mark_purged ON faucet_events;
DROP FUNCTION faucet_events_mark_purged();
DROP TRIGGER faucet_events_number ON faucet_events;
DROP FUNCTION faucet_events_number();
DROP FUNCTION faucet_line_purged_through(text, bigint, bigint);
DROP TABLE faucet_event_lines;

DROP INDEX faucet_events_owner_line;
DROP INDEX faucet_events_repo_line;
ALTER TABLE faucet_events DROP CONSTRAINT faucet_events_owner_seq_check, DROP COLUMN owner_seq, DROP COLUMN repo_seq;
