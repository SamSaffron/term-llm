# syntax=docker/dockerfile:1.4
FROM archlinux:latest
ARG TERM_LLM_VERSION=latest

# Full system update + essentials
RUN pacman -Syu --noconfirm && \
    pacman -S --noconfirm \
      base-devel \
      bash \
      bash-completion \
      zsh \
      curl \
      wget \
      jq \
      openssh \
      ca-certificates \
      sudo \
      vim \
      neovim \
      ripgrep \
      fd \
      which \
      man-db \
      go \
      rust \
      python \
      python-pip \
      github-cli \
      fzf \
      bat \
      eza \
      yq \
      sqlite \
      rsync \
      unzip \
      p7zip \
      btop \
      iproute2 \
      bind \
      ncurses \
      kitty-terminfo \
      tree \
      git-delta \
      duf \
      dust \
      git-lfs \
      nmap \
      openbsd-netcat \
      strace \
      gnupg \
      uv \
      nspr \
      nss \
      at-spi2-core \
      libxcomposite \
      libxdamage \
      libxrandr \
      libxkbcommon \
      mesa \
      libdrm \
      alsa-lib \
      pango \
      libcups \
      libxfixes \
      libxext \
      cairo \
      dbus && \
    pacman -Scc --noconfirm

# Playwright (Python) + Chromium headless shell — used by agent scripts for JS-heavy scraping.
# Arch Linux isn't officially supported by playwright install-deps (apt-only), so we install
# the required system libs above manually. Chromium is installed for the agent user.
RUN uv pip install playwright --system --break-system-packages

# Unprivileged user for normal agent work. It has passwordless sudo so root
# operations remain possible but explicit.
RUN useradd -m -s /bin/zsh -G wheel agent && \
    echo "agent ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/agent && \
    chmod 0440 /etc/sudoers.d/agent && \
    printf "%s\n" \
      "# term-llm shell niceties" \
      "alias tl=term-llm" \
      "if ! infocmp \"\$TERM\" >/dev/null 2>&1; then export TERM=xterm-256color; fi" \
      "setopt prompt_subst" \
      "autoload -Uz colors && colors" \
      "bindkey -e" \
      "bindkey '^?' backward-delete-char" \
      "bindkey '^H' backward-delete-char" \
      "PROMPT='%F{cyan}\${AGENT_NAME:-agent}%f:%F{yellow}%~%f %# '" \
      > /home/agent/.zshrc && \
    chown agent:agent /home/agent/.zshrc && \
    sudo -Hu agent playwright install chromium

# Unprivileged user for AUR builds (makepkg refuses root)
RUN useradd -m -G wheel aur && \
    echo "aur ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/aur && \
    chmod 0440 /etc/sudoers.d/aur

# yay (AUR helper)
RUN sudo -u aur bash -c " \
      git clone https://aur.archlinux.org/yay.git /tmp/yay && \
      cd /tmp/yay && \
      makepkg -si --noconfirm && \
      rm -rf /tmp/yay"

# runit is AUR-only on Arch
RUN sudo -u aur yay -S --noconfirm runit

# mise (polyglot version manager — node, python, ruby, etc.)
RUN curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh

# Activate mise for interactive shells and provide a short term-llm alias.
RUN echo 'eval "$(mise activate bash)"' >> /etc/bash.bashrc && \
    echo 'alias tl=term-llm' >> /etc/bash.bashrc && \
    mkdir -p /etc/zsh && \
    { \
      echo 'eval "$(mise activate zsh)"'; \
      echo 'alias tl=term-llm'; \
      echo 'if ! infocmp "$TERM" >/dev/null 2>&1; then export TERM=xterm-256color; fi'; \
      echo 'autoload -Uz compinit && compinit'; \
      echo 'compdef tl=term-llm 2>/dev/null || true'; \
      echo 'setopt prompt_subst'; \
      echo 'autoload -Uz colors && colors'; \
      echo 'bindkey -e'; \
      echo "bindkey '^?' backward-delete-char"; \
      echo "bindkey '^H' backward-delete-char"; \
      echo "PROMPT='%F{cyan}\${AGENT_NAME:-agent}%f:%F{yellow}%~%f %# '"; \
    } > /etc/zsh/zshrc && \
    printf '#!/bin/bash\neval "$(mise activate bash)"\n' > /etc/profile.d/mise.sh && \
    chmod +x /etc/profile.d/mise.sh

# Install term-llm from the official release installer.
RUN if [ "${TERM_LLM_VERSION}" = "latest" ]; then \
      curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | \
        TERM_LLM_INSTALL_DIR=/usr/local/bin sh; \
    else \
      curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | \
        TERM_LLM_INSTALL_DIR=/usr/local/bin sh -s -- --version "${TERM_LLM_VERSION}"; \
    fi

# Install shell completion for term-llm in interactive container shells.
RUN mkdir -p /usr/share/bash-completion/completions /usr/share/zsh/site-functions && \
    term-llm config completion bash > /usr/share/bash-completion/completions/term-llm && \
    term-llm config completion zsh > /usr/share/zsh/site-functions/_term-llm && \
    grep -q '/usr/share/bash-completion/bash_completion' /etc/bash.bashrc || \
      printf '\n# Enable bash-completion for interactive shells.\n[ -r /usr/share/bash-completion/bash_completion ] && . /usr/share/bash-completion/bash_completion\n' >> /etc/bash.bashrc

# Install Claude Code CLI so the claude-bin provider works inside the container.
# The official installer drops the binary in the invoking user's ~/.local/bin;
# install as agent and symlink to /usr/local/bin so services and shells find it.
RUN sudo -Hu agent bash -c 'curl -fsSL https://claude.ai/install.sh | bash' && \
    if [ ! -e /usr/local/bin/claude ] && [ -e /home/agent/.local/bin/claude ]; then \
      ln -sf /home/agent/.local/bin/claude /usr/local/bin/claude; \
    fi && \
    /usr/local/bin/claude --version

# Keep the term-llm source tree available for self-discovery and local hacking.
# The running agent is built on top of term-llm; exposing the source at the
# conventional ~/source/term-llm path makes that relationship easy to inspect.
RUN sudo -Hu agent mkdir -p /home/agent/source && \
    sudo -Hu agent git clone https://github.com/samsaffron/term-llm.git /home/agent/source/term-llm && \
    if [ "${TERM_LLM_VERSION}" != "latest" ]; then \
      cd /home/agent/source/term-llm && \
      sudo -Hu agent git checkout "${TERM_LLM_VERSION}" || sudo -Hu agent git checkout "v${TERM_LLM_VERSION}" || true; \
    fi

COPY bootstrap/ /opt/term-llm/bootstrap/
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh && \
    find /opt/term-llm/bootstrap/scripts -type f -name '*.sh' -exec chmod 755 {} + && \
    find /opt/term-llm/bootstrap/services -type f -name run -exec chmod 755 {} + && \
    mkdir -p /etc/sv /etc/runit/runsvdir

ENTRYPOINT ["/entrypoint.sh"]
