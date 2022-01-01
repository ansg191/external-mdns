#!/usr/bin/env bash

if [[ -n "$APP_VERSION" ]]; then
    docker buildx build \
        --tag blakec/external-mdns:latest \
        --tag "blakec/external-mdns:${APP_VERSION}" \
        --platform linux/amd64,linux/arm64 \
        --output=type=registry .
fi
