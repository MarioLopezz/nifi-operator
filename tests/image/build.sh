podman build . --tag docker.io/kubernetesbigdataeg/nifi:1.27.0-1
podman login docker.io -u kubernetesbigdataeg
podman push docker.io/kubernetesbigdataeg/nifi:1.27.0-1