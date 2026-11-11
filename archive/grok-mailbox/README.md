# Temporary Grok mailbox boundary

These files preserve the adapter-local mailbox interface and its original tests before
M1 integration. They are not compiled. Production Grok uses taskai_core::mail::Mailbox;
active tests in crates/taskai-connectors/tests/grok_mail.rs cover the same behavior through
private authenticated sockets and real core bookkeeping.
