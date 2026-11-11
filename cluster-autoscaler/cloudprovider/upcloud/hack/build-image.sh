#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Builds and pushes the cluster-autoscaler image with the UpCloud provider (kihoteam fork).
#
#   cloudprovider/upcloud/hack/build-image.sh build <build-number>
#   cloudprovider/upcloud/hack/build-image.sh push <build-number>
#
# Tag: v<CA version>-upcloud.<build-number>, e.g. v1.36.2-upcloud.1. Bump the build number
# for every image built from new fork commits on the same CA version.
# Env: IMAGE_REPOSITORY (default ghcr.io/kihoteam/autoscaler), CONTAINER_ENGINE (default docker).

set -o errexit
set -o nounset
set -o pipefail

ca_dir="$(cd "$(dirname "$0")/../../.." && pwd)"
image_repository="${IMAGE_REPOSITORY:-ghcr.io/kihoteam/autoscaler}"
container_engine="${CONTAINER_ENGINE:-docker}"

usage() {
    echo "Usage: $0 build|push <build-number>" >&2
    exit 1
}

image_version() {
    local build_number="$1"
    [[ "${build_number}" =~ ^[0-9]+$ ]] || usage
    echo "$("${ca_dir}/cloudprovider/upcloud/hack/version.sh")-upcloud.${build_number}"
}

require_clean_tree() {
    if ! git -C "${ca_dir}" diff --quiet HEAD -- . || [ -n "$(git -C "${ca_dir}" ls-files --others --exclude-standard -- .)" ]; then
        echo "Uncommitted changes in ${ca_dir}: commit them first so the image matches a commit." >&2
        exit 1
    fi
}

build_image() {
    local version
    version="$(image_version "$1")"
    require_clean_tree
    "${container_engine}" build \
        --platform linux/amd64 \
        --build-arg GOARCH=amd64 \
        --build-arg BUILD_TAGS=upcloud \
        --build-arg "LDFLAGS=-s -w -X k8s.io/autoscaler/cluster-autoscaler/version.ClusterAutoscalerVersion=${version}" \
        --label "org.opencontainers.image.source=https://github.com/kihoteam/autoscaler" \
        --label "org.opencontainers.image.revision=$(git -C "${ca_dir}" rev-parse HEAD)" \
        --label "org.opencontainers.image.version=v${version}" \
        -t "${image_repository}:v${version}" \
        -f "${ca_dir}/Dockerfile" \
        "${ca_dir}"
    echo "Built ${image_repository}:v${version}"
}

push_image() {
    local version
    version="$(image_version "$1")"
    "${container_engine}" push "${image_repository}:v${version}"
}

[ $# -eq 2 ] || usage
case "$1" in
    build) build_image "$2" ;;
    push) push_image "$2" ;;
    *) usage ;;
esac
