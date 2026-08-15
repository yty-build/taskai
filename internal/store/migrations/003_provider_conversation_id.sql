ALTER TABLE sessions ADD COLUMN provider_conversation_id TEXT;
ALTER TABLE sessions ADD COLUMN conversation_id_source TEXT;
ALTER TABLE sessions ADD COLUMN conversation_id_verified_at TEXT;

UPDATE sessions
SET provider_conversation_id = CASE
        WHEN COALESCE(native_session_id, '') <> '' THEN native_session_id
        ELSE acp_session_id
    END,
    conversation_id_source = CASE
        WHEN COALESCE(native_session_id, '') <> '' THEN 'native'
        WHEN COALESCE(acp_session_id, '') <> '' THEN 'acp'
        ELSE NULL
    END
WHERE COALESCE(provider_conversation_id, '') = ''
  AND (COALESCE(native_session_id, '') <> '' OR COALESCE(acp_session_id, '') <> '');
