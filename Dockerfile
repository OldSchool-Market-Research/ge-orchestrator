# ge-orchestrator: its own static binary plus the ge-agent and ge-mcp binaries
# it spawns, composed from their published images at PINNED tags (bump the ARGs
# deliberately — see each repo's VERSION). DIRECTIVE.md rides in from ge-agent.

ARG GE_AGENT_IMAGE=ghcr.io/oldschool-market-research/ge-agent:0.13.0
ARG GE_MCP_IMAGE=ghcr.io/oldschool-market-research/ge-mcp:0.7.0

FROM ${GE_AGENT_IMAGE} AS agent
FROM ${GE_MCP_IMAGE}   AS mcp

# ---- build ----
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/ge-orchestrator .

# ---- runtime ----
# Node rather than distroless/static: ge-agent's kimi driver
# (GE_AGENT_DRIVER=kimi) runs the Kimi Code CLI, a node app, pinned to the
# version validated against prod briefs. Optional deps are a PTY and a
# clipboard, neither used headless. The CLI's login, config and sessions live
# in GE_AGENT_KIMI_HOME on the state volume, not in the image.
FROM node:22.23.3-bookworm-slim
RUN npm install -g --omit=optional @moonshot-ai/kimi-code@2.3.1 \
 && npm cache clean --force
COPY --from=build /out/ge-orchestrator /ge-orchestrator
COPY --from=agent /ge-agent /bin/ge-agent
COPY --from=agent /etc/ge-agent/DIRECTIVE.md /etc/ge-agent/DIRECTIVE.md
COPY --from=mcp   /ge-mcp   /bin/ge-mcp
# The distroless nonroot uid: the /state hostPath on eldo is owned by it.
USER 65532:65532
ENV GE_AGENT_PATH=/bin/ge-agent \
    GE_MCP_PATH=/bin/ge-mcp \
    GE_AGENT_DIRECTIVE=/etc/ge-agent/DIRECTIVE.md \
    GE_AGENT_KIMI_CMD=/usr/local/bin/kimi \
    GE_ORCH_STATE=/state \
    GE_ORCH_ADDR=0.0.0.0:8410
ENTRYPOINT ["/ge-orchestrator"]
