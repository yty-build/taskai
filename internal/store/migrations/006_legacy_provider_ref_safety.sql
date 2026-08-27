-- Migration 003 predated the Session Channel contract and could manufacture a
-- provider_conversation_id by choosing between native_session_id and
-- acp_session_id. Those values are legacy evidence, not a provider-verified
-- canonical identity, and must never drive an exact Resume in the new model.
--
-- Any canonical/source/verification/native evidence marks a pre-Session-
-- Channel row. Preserve those columns as audit evidence, but clear the
-- physical ProviderRef regardless of the legacy verification claim. Rows
-- created by the Session Channel leave every legacy evidence field empty and
-- store their opaque ProviderRef directly in acp_session_id, so they are not
-- touched.
UPDATE sessions
SET acp_session_id = NULL
WHERE COALESCE(provider_conversation_id, '') <> ''
   OR COALESCE(conversation_id_source, '') <> ''
   OR conversation_id_verified_at IS NOT NULL
   OR COALESCE(native_session_id, '') <> '';

-- A row with no recorded worker cannot truthfully remain live across an
-- upgrade, including a development database that had already applied v5.
UPDATE sessions
SET status = 'stopped'
WHERE COALESCE(worker_pid, 0) = 0
  AND COALESCE(worker_socket, '') = ''
  AND status IN ('starting', 'waiting', 'working');
