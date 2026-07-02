## MODIFIED Requirements

### Requirement: Start web server
The serve command MUST start an HTTP server for the visualization interface with optional browser control.

#### Scenario: Start server on default port
- Given a git repository with stored conversations
- When the user runs `claudit serve`
- Then claudit starts an HTTP server on port 8080
- And displays the URL in the terminal

#### Scenario: Start server on custom port
- Given a git repository with stored conversations
- When the user runs `claudit serve --port 3000`
- Then claudit starts an HTTP server on port 3000

#### Scenario: Localhost binding by default
- Given the server is started without options
- When examining the server binding
- Then it binds to 127.0.0.1 (localhost only)

#### Scenario: Auto-open browser
- **WHEN** the user runs `claudit serve` without `--no-browser`
- **THEN** the default web browser is opened to the server URL

#### Scenario: Suppress browser opening
- **WHEN** the user runs `claudit serve --no-browser`
- **THEN** the server starts without opening a browser
- **AND** the URL is still printed to the terminal
