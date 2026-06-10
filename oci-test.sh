#!/usr/bin/env bash
set -e

pull_rmi() {
  docker pull "$1"
  docker rmi "$1"
}

pull_rmi dcr.mirs.uk/hello-world:latest
pull_rmi ghcr.mirs.uk/astral-sh/uv:latest
pull_rmi gcr.mirs.uk/distroless/static:latest
pull_rmi k8s.mirs.uk/pause:3.9
pull_rmi quay.mirs.uk/prometheus/busybox:latest
pull_rmi mcr.mirs.uk/mcr/hello-world:latest
pull_rmi nvcr.mirs.uk/nvidia/cuda:12.2.0-base-ubuntu22.04
pull_rmi ocr.mirs.uk/os/oraclelinux:8-slim
