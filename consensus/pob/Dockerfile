# Dockerfile
# Multi-stage build for the PoB-patched geth binary.
# Base: BSC's go-ethereum fork (bnb-chain/bsc) with our PoB patches applied.

# ── Stage 1: builder ──────────────────────────────────────────────────────────
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache gcc musl-dev linux-headers git make

WORKDIR /src

# Clone BNB Chain (BSC) — use the tag that matches your target version
ARG BSC_TAG=v1.4.7
RUN git clone --depth 1 --branch ${BSC_TAG} \
    https://github.com/bnb-chain/bsc.git .

# Copy our custom consensus engine and params override
COPY consensus/pob/ ./consensus/pob/
COPY params/pob_config.go ./params/pob_config.go

# Build
RUN make geth

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM alpine:3.18

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /src/build/bin/geth /usr/local/bin/geth

EXPOSE 8545 8546 30303 30303/udp

ENTRYPOINT ["geth"]
