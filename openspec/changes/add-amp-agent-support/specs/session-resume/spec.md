## MODIFIED Requirements

### Requirement: Checkout and launch coding agent
The resume command MUST checkout the commit and launch the configured coding agent, with a force option to skip confirmation.

#### Scenario: Launch Amp with thread
- **Given** a restored session with agent `"amp"` and a thread ID
- **When** resuming the session
- **Then** shiftlog launches `amp threads continue <threadId>`

### Requirement: Restore transcript to agent location
The resume command MUST write the transcript where the configured agent expects it.

#### Scenario: Amp session restore (no local transcript)
- **Given** a decompressed transcript with agent `"amp"`
- **When** resuming the session
- **Then** shiftlog instructs the user to run `amp threads continue <threadId>` since Amp threads are cloud-hosted
- **AND** no local transcript file is written (Amp threads sync from Sourcegraph servers)
