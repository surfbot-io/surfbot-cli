# surfbot-cli e2e validation image.
#
# Runs `surfbot-cli` under systemd inside a privileged Ubuntu container.
# Orchestrated from outside by Claude Code per `test/e2e/e2e-runbook.md`;
# each runbook step is a `docker exec` into this container invoking a
# subcommand of `container-internal.sh`.
#
# REQUIRED runtime flags — without these systemd refuses to come up:
#   --privileged
#   -v /sys/fs/cgroup:/sys/fs/cgroup:rw
#
# Typical invocation (see runbook for the full form):
#   docker build -t surfbot-e2e-img test/e2e/
#   docker run --rm -d --name surfbot-e2e \
#     --privileged \
#     -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
#     -v "$(pwd)/bin/surfbot-cli:/usr/local/bin/surfbot-cli:ro" \
#     -v "$(pwd)/test/e2e/container-internal.sh:/opt/e2e/internal.sh:ro" \
#     surfbot-e2e-img
#
# The cli binary and the internal helper script are mounted (not COPYd)
# so a `make build` on the host instantly reflects in the container
# without rebuilding the image.

# jrei/systemd-ubuntu is a well-maintained community image that wires
# systemd as PID 1 with the right tmpfs/cgroup hooks. The 24.04 tag is
# pinned to the Ubuntu LTS family; consumers should re-pin to a digest
# (`@sha256:...`) once they've vetted a specific build for their CI.
#
# The published manifest is amd64-only, so we force linux/amd64. On
# Apple Silicon hosts this runs under qemu emulation — slower but
# fully functional, and it keeps a single x86_64 cli binary covering
# both arm64 Macs and amd64 Linux surfbots.
FROM --platform=linux/amd64 jrei/systemd-ubuntu:24.04

# iptables is required by the network_cut subcommand (SPEC §15 step 7).
# curl + ca-certificates are needed by the cli to reach api-staging over
# TLS. sudo lets the unprivileged surfbot user invoke `daemon install`
# and the iptables rules without juggling capabilities.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        iptables \
        curl \
        ca-certificates \
        sudo \
    && rm -rf /var/lib/apt/lists/*

# Non-root surfbot user. NOPASSWD sudo keeps the runbook commands
# clean — every `docker exec -u surfbot` step can elevate when needed
# without prompting.
RUN useradd -m -s /bin/bash surfbot && \
    echo "surfbot ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/surfbot && \
    chmod 0440 /etc/sudoers.d/surfbot

# Mount point for the helper script (mounted at runtime, not baked in).
RUN mkdir -p /opt/e2e && chown surfbot:surfbot /opt/e2e

# Inherit CMD from the base image so systemd runs as PID 1.
