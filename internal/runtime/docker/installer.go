package docker

const installerOwnerLabel = "ky.managed-by"

func installerManaged(labels map[string]string) bool {
	return labels[installerOwnerLabel] == "kyquickstart"
}
