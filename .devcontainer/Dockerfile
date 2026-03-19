FROM mcr.microsoft.com/devcontainers/go:1-bookworm

# libcap2-bin  — capsh & setcap for ambient capability wrappers
# gettext-base — envsubst, used by tests/e2e-sh/run.sh
RUN apt-get update && apt-get install -y --no-install-recommends \
        libcap2-bin \
        gettext-base \
    && rm -rf /var/lib/apt/lists/*

# Capability wrapper scripts ---------------------------------------------------
COPY scripts/cap-wrapper.sh      /usr/local/bin/cap-wrapper
COPY scripts/go-cap-wrapper.sh   /usr/local/bin/go-cap-wrapper
COPY scripts/dlv-cap-wrapper.sh  /usr/local/bin/dlv-cap-wrapper
RUN chmod +x /usr/local/bin/cap-wrapper \
              /usr/local/bin/go-cap-wrapper \
              /usr/local/bin/dlv-cap-wrapper \
    && ln -sf /usr/local/bin/go-cap-wrapper /usr/local/bin/go

# Register the capability wrapper as a valid login shell and use it for
# the vscode user so that every integrated-terminal session automatically
# inherits CAP_NET_ADMIN + CAP_NET_BIND_SERVICE.
RUN echo '/usr/local/bin/cap-wrapper' >> /etc/shells \
    && chsh -s /usr/local/bin/cap-wrapper vscode
