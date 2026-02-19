FROM golang:1.25 AS builder
WORKDIR /build
COPY .. .
RUN go build -o term-llm .

FROM archlinux:latest

# Full system update + essentials
RUN pacman -Syu --noconfirm && \
    pacman -S --noconfirm \
      base-devel \
      git \
      bash \
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
      runit && \
    pacman -Scc --noconfirm

# Unprivileged user for AUR builds (makepkg refuses root)
RUN useradd -m -G wheel aur && \
    echo "aur ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/aur

# yay (AUR helper)
RUN sudo -u aur bash -c " \
      git clone https://aur.archlinux.org/yay.git /tmp/yay && \
      cd /tmp/yay && \
      makepkg -si --noconfirm && \
      rm -rf /tmp/yay"

# mise (polyglot version manager — node, python, ruby, etc.)
RUN curl https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh

# Activate mise for all bash sessions (login and interactive)
RUN echo 'eval "$(mise activate bash)"' >> /etc/bash.bashrc && \
    printf '#!/bin/bash\neval "$(mise activate bash)"\n' > /etc/profile.d/mise.sh && \
    chmod +x /etc/profile.d/mise.sh

COPY --from=builder /build/term-llm /usr/local/bin/term-llm

COPY docker/jarvis/agent.yaml /bootstrap/agent.yaml
COPY docker/jarvis/system.md /bootstrap/system.md
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# runit service skeleton — actual services are symlinked at runtime
# (either by entrypoint or by the user via docker exec)
# Drop your service dirs under docker/sv/ and they'll be available at /etc/sv/
COPY docker/sv/ /etc/sv/

ENTRYPOINT ["/entrypoint.sh"]
