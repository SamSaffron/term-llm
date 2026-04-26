# syntax=docker/dockerfile:1.4
FROM archlinux:latest
ARG TERM_LLM_VERSION=latest

# Full system update + essentials
RUN pacman -Syu --noconfirm && \
    pacman -S --noconfirm \
      base-devel \
      bash \
      bash-completion \
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
# the required system libs above manually. Chromium goes to /root/.cache/ms-playwright/.
RUN uv pip install playwright --system --break-system-packages && \
    playwright install chromium

# Unprivileged user for AUR builds (makepkg refuses root)
RUN useradd -m -G wheel aur && \
    echo "aur ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/aur

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

# Activate mise for all bash sessions (login and interactive)
RUN echo 'eval "$(mise activate bash)"' >> /etc/bash.bashrc && \
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

# Install bash completion for term-llm in interactive container shells.
RUN mkdir -p /usr/share/bash-completion/completions && \
    term-llm config completion bash > /usr/share/bash-completion/completions/term-llm && \
    grep -q '/usr/share/bash-completion/bash_completion' /etc/bash.bashrc || \
      printf '\n# Enable bash-completion for interactive shells.\n[ -r /usr/share/bash-completion/bash_completion ] && . /usr/share/bash-completion/bash_completion\n' >> /etc/bash.bashrc

# Install Claude Code CLI so the claude-bin provider works inside the container.
# The official installer drops the binary in /root/.local/bin; we symlink to
# /usr/local/bin so runit-managed services and non-login shells find it.
RUN curl -fsSL https://claude.ai/install.sh | bash && \
    if [ ! -e /usr/local/bin/claude ] && [ -e /root/.local/bin/claude ]; then \
      ln -sf /root/.local/bin/claude /usr/local/bin/claude; \
    fi && \
    /usr/local/bin/claude --version

# Keep the term-llm source tree available for self-discovery and local hacking.
# The running agent is built on top of term-llm; exposing the source at the
# conventional ~/source/term-llm path makes that relationship easy to inspect.
RUN mkdir -p /root/source && \
    git clone https://github.com/samsaffron/term-llm.git /root/source/term-llm && \
    if [ "${TERM_LLM_VERSION}" != "latest" ]; then \
      cd /root/source/term-llm && \
      git checkout "${TERM_LLM_VERSION}" || git checkout "v${TERM_LLM_VERSION}" || true; \
    fi

COPY bootstrap/ /opt/term-llm/bootstrap/
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh && \
    find /opt/term-llm/bootstrap/scripts -type f -name '*.sh' -exec chmod 755 {} + && \
    find /opt/term-llm/bootstrap/services -type f -name run -exec chmod 755 {} + && \
    mkdir -p /etc/sv /etc/runit/runsvdir

ENTRYPOINT ["/entrypoint.sh"]
