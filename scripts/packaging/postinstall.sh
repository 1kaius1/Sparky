#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# nfpm postinstall scriptlet (deb postinst / rpm %post) - see
# scripts/packaging/nfpm.yaml. Sources the *installed* copy of
# agent-common.sh (see that file's own doc comment for why) to perform the
# same account/group/secrets-file setup every install method needs.
set -e

. /opt/sparky/share/sparky-agent/agent-common.sh

ensure_serviceloop_user
ensure_model_storage_dir
ensure_gpu_group_membership
ensure_secrets_file /opt/sparky/share/sparky-agent/secrets.env.template

systemctl daemon-reload
systemctl enable sparky-agent >/dev/null 2>&1 || true

# Never auto-start on a fresh install - an unconfigured secrets.env would
# just crash-loop. On a fresh install nothing is active yet, so this check
# is always false. On an upgrade, the old process is still running at this
# point (preremove.sh deliberately no-ops on upgrade rather than stopping
# the service first), so this correctly restarts it onto the new binary -
# safe per docs/AGENT.md's own Signal Handling reasoning: an agent process
# restart doesn't disrupt an already-loaded model on the container-runtime
# backend, since the container is managed by the container runtime daemon
# independent of the agent's own process lifetime.
if systemctl is-active --quiet sparky-agent 2>/dev/null; then
    systemctl restart sparky-agent
else
    echo "sparky-agent installed but not started."
    echo "Fill in /etc/sparky-agent/secrets.env, then run: sudo systemctl start sparky-agent"
fi
