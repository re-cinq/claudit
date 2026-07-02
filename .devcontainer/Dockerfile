FROM node:20

ARG TZ
ENV TZ="$TZ"

ARG CLAUDE_CODE_VERSION=latest

# Install basic development tools and iptables/ipset
RUN apt-get update && apt-get install -y --no-install-recommends \
  less \
  git \
  procps \
  sudo \
  fzf \
  zsh \
  man-db \
  unzip \
  gnupg2 \
  gh \
  jq \
  nano \
  vim \
  && apt-get clean && rm -rf /var/lib/apt/lists/*

# Install dependencies for Chrome/Chromium (needed for chrome-devtools MCP server)
RUN apt-get update && apt-get install -y --no-install-recommends \
  libnss3 \
  libnspr4 \
  libatk1.0-0 \
  libatk-bridge2.0-0 \
  libcups2 \
  libdrm2 \
  libxkbcommon0 \
  libxcomposite1 \
  libxdamage1 \
  libxfixes3 \
  libxrandr2 \
  libgbm1 \
  libasound2 \
  libpango-1.0-0 \
  libcairo2 \
  libatspi2.0-0 \
  fonts-liberation \
  libappindicator3-1 \
  xdg-utils \
  && apt-get clean && rm -rf /var/lib/apt/lists/*

# Ensure default node user has access to /usr/local/share
RUN mkdir -p /usr/local/share/npm-global && \
  chown -R node:node /usr/local/share

ARG USERNAME=node

# Persist bash history.
RUN SNIPPET="export PROMPT_COMMAND='history -a' && export HISTFILE=/commandhistory/.bash_history" \
  && mkdir /commandhistory \
  && touch /commandhistory/.bash_history \
  && chown -R $USERNAME /commandhistory

# Set `DEVCONTAINER` environment variable to help with orientation
ENV DEVCONTAINER=true

# Create workspace and config directories and set permissions
RUN mkdir -p /workspace /home/node/.claude /home/node/go && \
  chown -R node:node /workspace /home/node/.claude /home/node/go

WORKDIR /workspace

ARG GIT_DELTA_VERSION=0.18.2
RUN ARCH=$(dpkg --print-architecture) && \
  wget "https://github.com/dandavison/delta/releases/download/${GIT_DELTA_VERSION}/git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb" && \
  sudo dpkg -i "git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb" && \
  rm "git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb"

# Install Go
ARG GO_VERSION=1.25.6
RUN ARCH=$(dpkg --print-architecture) && \
  if [ "$ARCH" = "amd64" ]; then GOARCH="amd64"; else GOARCH="arm64"; fi && \
  wget "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" && \
  tar -C /usr/local -xzf "go${GO_VERSION}.linux-${GOARCH}.tar.gz" && \
  rm "go${GO_VERSION}.linux-${GOARCH}.tar.gz"

ENV PATH=$PATH:/usr/local/go/bin
ENV GOPATH=/home/node/go
ENV PATH=$PATH:/home/node/go/bin

# Install Playwright globally and set up Chromium for chrome-devtools MCP server
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
RUN npm install -g playwright && \
  npx playwright install chromium && \
  chmod -R 755 /opt/ms-playwright

# Create the expected Chrome directory structure and symlink for MCP server
RUN mkdir -p /opt/google/chrome && \
  CHROMIUM_PATH=$(find /opt/ms-playwright -name chrome -type f 2>/dev/null | head -1) && \
  ln -s "$CHROMIUM_PATH" /opt/google/chrome/chrome

# Create a script to locate and run Chromium
RUN echo '#!/bin/bash\n\
CHROMIUM_PATH=$(find /opt/ms-playwright -name chrome -type f 2>/dev/null | head -1)\n\
if [ -z "$CHROMIUM_PATH" ]; then\n\
  echo "Chromium not found" >&2\n\
  exit 1\n\
fi\n\
exec "$CHROMIUM_PATH" "$@"' > /usr/local/bin/chromium && \
  chmod +x /usr/local/bin/chromium

# Set environment variables for Chromium
ENV CHROME_BIN=/usr/local/bin/chromium
ENV PUPPETEER_SKIP_CHROMIUM_DOWNLOAD=true
ENV PUPPETEER_EXECUTABLE_PATH=/usr/local/bin/chromium

# Set up non-root user
USER node

# Install global packages
ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=$PATH:/usr/local/share/npm-global/bin

# Set the default shell to zsh rather than sh
ENV SHELL=/bin/zsh

# Set the default editor and visual
ENV EDITOR=nano
ENV VISUAL=nano

# Default powerline10k theme
ARG ZSH_IN_DOCKER_VERSION=1.2.0
RUN sh -c "$(wget -O- https://github.com/deluan/zsh-in-docker/releases/download/v${ZSH_IN_DOCKER_VERSION}/zsh-in-docker.sh)" -- \
  -p git \
  -p fzf \
  -a "source /usr/share/doc/fzf/examples/key-bindings.zsh" \
  -a "source /usr/share/doc/fzf/examples/completion.zsh" \
  -a "export PROMPT_COMMAND='history -a' && export HISTFILE=/commandhistory/.bash_history" \
  -x

# Install Claude Code
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}

# Install coding agent CLIs (for integration tests)
RUN npm install -g @openai/codex \
  && npm install -g @github/copilot \
  && npm install -g @google/gemini-cli \
  && npm install -g opencode-ai@latest

# Install act (run GitHub Actions locally, requires Docker-in-Docker feature)
RUN curl -sSf https://raw.githubusercontent.com/nektos/act/master/install.sh | bash -s -- -b /home/node/.local/bin

# Install OpenSpec (spec-driven development for AI coding assistants)
RUN npm install -g @fission-ai/openspec@latest

# Install Beads (memory/issue tracking for coding agents)
# Create .local/bin and add to PATH before running installer
ENV PATH=$PATH:/home/node/.local/bin
RUN mkdir -p /home/node/.local/bin && \
    curl -fsSL https://raw.githubusercontent.com/steveyegge/beads/main/scripts/install.sh | bash

# Install golangci-lint (overrides bundled version from beads to match Go version)
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b /home/node/.local/bin latest

