# Host Roster Contract Request

Status: **Deferred: requires explicit user authorization**

No remote issue, pull request, message, or other submission has been made.
Capability-A and Capability-B are both implemented locally, so this deferred
request does not block S1.

## Requested contract

Please formalize `host.auth.list` as the authoritative complete authentication
roster and guarantee that every returned entry includes an explicit integer
`Priority`, including priority zero. The response should be one coherent
snapshot rather than a request-filtered candidate subset.

For safe synchronization and writeback, please additionally expose:

1. A non-reusable instance incarnation and monotonically increasing roster
   revision, so plugin state cannot be confused across host restarts.
2. A tombstone/change sequence that makes deletions and ordered changes
   observable without inferring absence from partial responses.
3. `SaveAuth(expected_revision)` compare-and-swap semantics, returning a
   distinct conflict result when the supplied revision is stale.

The v7.2.42 typed `HostAuthFileEntry.Priority int` cannot distinguish a missing
JSON field from an explicit zero. Until the contract is formalized, the plugin
preserves field presence in its narrow raw-JSON normalization boundary and
uses Capability-B when any roster entry omits Priority.
