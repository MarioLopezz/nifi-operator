FROM openjdk:8u201-jre-alpine

ARG UID=1000
ARG GID=1000
ARG NIFI_VERSION=1.16.1
ARG BASE_URL=https://archive.apache.org/dist
ARG MIRROR_BASE_URL=${MIRROR_BASE_URL:-${BASE_URL}}
ARG DISTRO_PATH=${DISTRO_PATH:-${NIFI_VERSION}}
ARG NIFI_BINARY_PATH=${NIFI_BINARY_PATH:-/nifi/${DISTRO_PATH}/nifi-${NIFI_VERSION}-bin.zip}
ARG NIFI_TOOLKIT_BINARY_PATH=${NIFI_TOOLKIT_BINARY_PATH:-/nifi/${DISTRO_PATH}/nifi-toolkit-${NIFI_VERSION}-bin.zip}

ENV NIFI_BASE_DIR=/opt/nifi
ENV NIFI_HOME ${NIFI_BASE_DIR}/nifi-current
ENV NIFI_TOOLKIT_HOME ${NIFI_BASE_DIR}/nifi-toolkit-current

ENV NIFI_PID_DIR=${NIFI_HOME}/run
ENV NIFI_LOG_DIR=${NIFI_HOME}/logs

COPY scripts ${NIFI_BASE_DIR}/scripts/
RUN chmod -R +x ${NIFI_BASE_DIR}/scripts/*.sh
RUN apk update \
    && apk add bash \
    && wget -O  ${NIFI_BASE_DIR}/propgen  https://github.com/javiroman/propgen/releases/download/release-v0.0.2/propgen \
    && chmod 777 ${NIFI_BASE_DIR}/propgen

# Setup NiFi user and create necessary directories
#RUN addgroup -g ${GID} nifi || groupmod -n nifi `getent group ${GID} | cut -d: -f1` 
RUN adduser -D -h /home/nifi nifi \
	&& [ `id -u nifi` -eq 1000 ] \
	&& [ `id -g nifi` -eq 1000 ] 
RUN mkdir -p ${NIFI_BASE_DIR} \
    && chown -R nifi:nifi ${NIFI_BASE_DIR} \
    && apk update \
    && apk add -y jq xmlstarlet procps \
    && apk add curl

# Download, validate, and expand Apache NiFi Toolkit binary.
RUN curl -fSL ${MIRROR_BASE_URL}/${NIFI_TOOLKIT_BINARY_PATH} -o ${NIFI_BASE_DIR}/nifi-toolkit-${NIFI_VERSION}-bin.zip \
    && echo "$(curl ${BASE_URL}/${NIFI_TOOLKIT_BINARY_PATH}.sha256) *${NIFI_BASE_DIR}/nifi-toolkit-${NIFI_VERSION}-bin.zip" | sha256sum -c - \
    && unzip ${NIFI_BASE_DIR}/nifi-toolkit-${NIFI_VERSION}-bin.zip -d ${NIFI_BASE_DIR} \
    && rm ${NIFI_BASE_DIR}/nifi-toolkit-${NIFI_VERSION}-bin.zip \
    && mv ${NIFI_BASE_DIR}/nifi-toolkit-${NIFI_VERSION} ${NIFI_TOOLKIT_HOME} \
    && ln -s ${NIFI_TOOLKIT_HOME} ${NIFI_BASE_DIR}/nifi-toolkit-${NIFI_VERSION}

# Download, validate, and expand Apache NiFi binary.
RUN curl -fSL ${MIRROR_BASE_URL}/${NIFI_BINARY_PATH} -o ${NIFI_BASE_DIR}/nifi-${NIFI_VERSION}-bin.zip \
    && echo "$(curl ${BASE_URL}/${NIFI_BINARY_PATH}.sha256) *${NIFI_BASE_DIR}/nifi-${NIFI_VERSION}-bin.zip" | sha256sum -c - \
    && unzip ${NIFI_BASE_DIR}/nifi-${NIFI_VERSION}-bin.zip -d ${NIFI_BASE_DIR} \
    && rm ${NIFI_BASE_DIR}/nifi-${NIFI_VERSION}-bin.zip \
    && mv ${NIFI_BASE_DIR}/nifi-${NIFI_VERSION} ${NIFI_HOME} \
    && mkdir -p ${NIFI_HOME}/conf \
    && mkdir -p ${NIFI_HOME}/database_repository \
    && mkdir -p ${NIFI_HOME}/flowfile_repository \
    && mkdir -p ${NIFI_HOME}/content_repository \
    && mkdir -p ${NIFI_HOME}/provenance_repository \
    && mkdir -p ${NIFI_HOME}/state \
    && mkdir -p ${NIFI_LOG_DIR} \
    && ln -s ${NIFI_HOME} ${NIFI_BASE_DIR}/nifi-${NIFI_VERSION}

VOLUME ${NIFI_LOG_DIR} \
       ${NIFI_HOME}/conf \
       ${NIFI_HOME}/database_repository \
       ${NIFI_HOME}/flowfile_repository \
       ${NIFI_HOME}/content_repository \
       ${NIFI_HOME}/provenance_repository \
       ${NIFI_HOME}/state

# Clear nifi-env.sh in favour of configuring all environment variables in the Dockerfile
RUN echo "#!/bin/sh\n" > $NIFI_HOME/bin/nifi-env.sh

# Web HTTP(s) & Socket Site-to-Site Ports
EXPOSE 8080 8443 10000 8000

WORKDIR ${NIFI_HOME}

ENTRYPOINT ["../scripts/start.sh"]