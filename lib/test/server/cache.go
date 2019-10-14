package server

import (
	"github.com/bradfitz/gomemcache/memcache"
	"github.com/ory/dockertest"
	"log"
)

func Memcached(pool *dockertest.Pool) (closer func(), hostPort string, ipAddress string, err error) {
	log.Println("start memcached")
	mem, err := pool.Run("memcached", "1.5.12-alpine", []string{})
	if err != nil {
		return func() {}, "", "", err
	}
	hostPort = mem.GetPort("11211/tcp")
	err = pool.Retry(func() error {
		log.Println("try memcache connection...")
		_, err := memcache.New(mem.Container.NetworkSettings.IPAddress + ":11211").Get("foo")
		if err == memcache.ErrCacheMiss {
			return nil
		}
		if err != nil {
			log.Println(err)
		}
		return err
	})
	return func() { mem.Close() }, hostPort, mem.Container.NetworkSettings.IPAddress, err
}
