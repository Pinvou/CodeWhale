DROP TRIGGER event_replay_requests_immutable ON event_replay_requests;
DROP FUNCTION reject_event_replay_audit_mutation();
DROP TABLE event_replay_requests;
