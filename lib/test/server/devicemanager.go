package server

import (
	"github.com/ory/dockertest"
	"log"
	"net/http"
)

func DeviceManager(pool *dockertest.Pool, zk string, deviceRepoUrl string, semanticRepoUrl string, permsearchUrl string) (closer func(), hostPort string, ipAddress string, err error) {
	log.Println("start device repo")
	repo, err := pool.Run("fgseitsrancher.wifa.intern.uni-leipzig.de:5000/device-manager", "dev", []string{
		"ZOOKEEPER_URL=" + zk,
		"PERMISSIONS_URL=" + permsearchUrl,
		"DEVICE_REPO_URL=" + deviceRepoUrl,
		"SEMANTIC_REPO_URL=" + semanticRepoUrl,
	})
	if err != nil {
		return func() {}, "", "", err
	}
	hostPort = repo.GetPort("8080/tcp")
	err = pool.Retry(func() error {
		log.Println("try repo connection...")
		_, err := http.Get("http://" + repo.Container.NetworkSettings.IPAddress + ":8080/")
		if err != nil {
			log.Println(err)
		}
		return err
	})
	return func() { repo.Close() }, hostPort, repo.Container.NetworkSettings.IPAddress, err
}
