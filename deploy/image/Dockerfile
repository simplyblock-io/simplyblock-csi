# SPDX-License-Identifier: Apache-2.0
# Copyright (c) Arm Limited and Contributors
# Copyright (c) Intel Corporation
#
# XXX: pin alpine to 3.8 with e2fsprogs-1.44
# e2fsprogs-1.45+ crashes my test vm when running mkfs.ext4
FROM alpine:3.22
LABEL maintainers="SPDK-CSI Authors"
LABEL description="SPDK-CSI Plugin"

# Add a non-root system user
RUN addgroup -S spdkcsi && adduser -S -G spdkcsi spdkcsi && \
    apk add --no-cache \
        blkid \
        e2fsprogs \
        e2fsprogs-extra \
        nvme-cli \
        open-iscsi \
        util-linux \
        xfsprogs \
        xfsprogs-extra

COPY --chown=spdkcsi:spdkcsi spdkcsi /usr/local/bin/spdkcsi
RUN chmod 0755 /usr/local/bin/spdkcsi

# Drop to non-root user
USER spdkcsi

ENTRYPOINT ["/usr/local/bin/spdkcsi"]
