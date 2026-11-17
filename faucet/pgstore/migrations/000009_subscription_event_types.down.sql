DROP TRIGGER faucet_subscriptions_default_event_types ON faucet_subscriptions;
DROP FUNCTION faucet_subscriptions_default_event_types();

ALTER TABLE faucet_subscriptions DROP COLUMN event_types;
