#!/bin/sh
set -eu
if [ "$(/usr/bin/id -u)" != 0 ]; then
  exec /usr/bin/sudo -n -- @ROOTFUL_PODMAN_WRAPPER@ "$@"
fi
unset CONTAINER_HOST CONTAINER_CONNECTION DOCKER_HOST
unset CONTAINERS_CONF CONTAINERS_CONF_OVERRIDE CONTAINERS_STORAGE_CONF PODMAN_CONNECTIONS_CONF
unset XDG_CONFIG_HOME XDG_DATA_HOME XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS
HOME=/root
USER=root
LOGNAME=root
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export HOME USER LOGNAME PATH
exec /usr/bin/podman --remote=false "$@"
