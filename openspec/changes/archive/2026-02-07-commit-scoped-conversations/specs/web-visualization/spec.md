## ADDED Requirements

### Requirement: Incremental API Parameter
The conversation detail API SHALL support an `incremental` query parameter.

#### Scenario: Get incremental conversation
- **WHEN** client requests `GET /api/commits/<sha>?incremental=true`
- **AND** a parent commit has a stored conversation
- **THEN** response includes only entries since parent's last entry
- **AND** response includes `is_incremental: true`
- **AND** response includes `parent_commit_sha` field

#### Scenario: Get full conversation (default)
- **WHEN** client requests `GET /api/commits/<sha>` without incremental parameter
- **THEN** response includes full transcript (backward compatible)
- **AND** response includes `is_incremental: false`

#### Scenario: Incremental with no parent conversation
- **WHEN** client requests `GET /api/commits/<sha>?incremental=true`
- **AND** no parent commits have stored conversations
- **THEN** response includes full transcript
- **AND** response includes `is_incremental: false`
- **AND** `parent_commit_sha` is empty

### Requirement: Response Schema Extension
The conversation response SHALL include incremental display metadata including `is_incremental`, `parent_commit_sha`, and `incremental_count` fields.

#### Scenario: Incremental response includes metadata
- **WHEN** the API returns an incremental conversation response
- **THEN** the response includes `is_incremental: true`, `parent_commit_sha`, and `incremental_count`

### Requirement: UI Toggle
The web UI SHALL provide a toggle to switch between incremental and full conversation view.

#### Scenario: Toggle incremental/full
- **WHEN** user views a conversation in the web UI
- **THEN** a toggle is available to switch between incremental and full view
- **AND** incremental is the default when parent conversation exists
