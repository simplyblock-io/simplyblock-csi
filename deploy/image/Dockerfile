# SPDX-License-Identifier: Apache-2.0
# Copyright (c) Arm Limited and Contributors
# Copyright (c) Intel Corporation
#
# XXX: pin alpine to 3.8 with e2fsprogs-1.44
# e2fsprogs-1.45+ crashes my test vm when running mkfs.ext4
FROM alpine:3.18
LABEL maintainers="SPDK-CSI Authors"
LABEL description="SPDK-CSI Plugin"

COPY spdkcsi /usr/local/bin/spdkcsi

RUN apk update && \
    apk add nvme-cli open-iscsi e2fsprogs xfsprogs blkid xfsprogs-extra e2fsprogs-extra util-linux

ENTRYPOINT ["/usr/local/bin/spdkcsi"]

