# The Elephant User API

Service designed to manage user-centric data within the Elephant ecosystem.
It handles communication channels like inbox messages and system notifications,
as well as persistent user configurations through structured settings and simple properties.

### Features

- **Inbox Messages**: Store and retrieve user-specific documents intended for an inbox.
- **System Messages**: A secondary channel for system-level notifications and events.
- **Settings Management**:
  - **Documents**: Structured configurations validated against schemas using the `revisor` lib.
  - **Properties**: Lightweight key-value pairs for simple user preferences.
  - **Event Log**: A unified log of changes to documents and properties, supporting long-polling for efficient client synchronization.

### Shared Access Control

The service implements a multi-tier access model based on JWT claims for documents:

- **Private**: Documents and properties owned by the user (`sub` claim).
- **Shared**: Documents can be owned by an Organization (`org` claim) or a Unit (`units` claim), allowing shared access within those groups.
- **Permissions**: 
  - Standard users can read and write their own data and read shared data.
  - Users with the `doc_admin` scope can manage shared data across their organization and units.

### Schemas

Setting documents are validated against [revisor](https://github.com/ttab/revisor) constraint sets embedded in the binary from `internal/schema_*.json`:

- `internal/schema_messages.json` — constraints for inbox message payloads.
- `internal/schema_settings.json` — constraints for setting documents.

Adding a new setting type today requires editing `schema_settings.json` and rebuilding the service. The plan is to support dynamic settings schema registration through the user API so that schemas can be added at runtime without redeploying.
