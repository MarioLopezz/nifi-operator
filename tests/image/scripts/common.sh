#!/bin/sh -e
prop_replace () {
  target_file=${3:-${nifi_props_file}}
  echo 'replacing target file ' ${target_file}
  sed -i -e "s|^$1=.*$|$1=$2|"  ${target_file}
}

uncomment() {
	target_file=${2}
	echo "Uncommenting ${target_file}"
	sed -i -e "s|^\#$1|$1|" ${target_file}
}

# NIFI_HOME is defined by an ENV command in the backing Dockerfile
export nifi_bootstrap_file=${NIFI_HOME}/conf/bootstrap.conf
export nifi_props_file=${NIFI_HOME}/conf/nifi.properties
export nifi_toolkit_props_file=${HOME}/.nifi-cli.nifi.properties
export hostname=$(hostname)