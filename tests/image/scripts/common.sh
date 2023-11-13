#!/bin/sh -e

prop_replace () {
  target_file=${3:-${nifi_props_file}}
  echo "File [${target_file}] replacing [${1}]"
  sed -i -e "s|^$1=.*$|$1=$2|"  ${target_file}
}

uncomment() {
  target_file=${2}
  echo "File [${target_file}] uncommenting [${1}]"
  sed -i -e "s|^\#$1|$1|" ${target_file}
}


prop_add_or_replace () {
  target_file=${3:-${nifi_props_file}}
  property_found=$(awk -v property="${1}" 'index($0, property) == 1')
  if [ -z "${property_found}" ]; then
    echo "File [${target_file}] adding [${1}]"
    echo "$1=$2" >> ${target_file}
  else
    prop_replace $1 $2 $3  
  fi
}

# NIFI_HOME is defined by an ENV command in the backing Dockerfile
export nifi_bootstrap_file=${NIFI_HOME}/conf/bootstrap.conf
export nifi_props_file=${NIFI_HOME}/conf/nifi.properties
export nifi_toolkit_props_file=${HOME}/.nifi-cli.nifi.properties
export hostname=$(hostname)