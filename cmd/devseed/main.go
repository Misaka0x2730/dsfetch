// Command devseed adds the dev/docker-compose.yml test servers to a data
// directory, so `task run` has something to connect to right away.
//
//	go run ./cmd/devseed            # writes ./devdata/servers.json
//	go run ./cmd/devseed -host 192.168.1.20 -data /tmp/d
package main

import (
	"flag"
	"fmt"
	"log"

	"dsfetch/internal/store"
)

func main() {
	data := flag.String("data", "devdata", "data directory")
	host := flag.String("host", "127.0.0.1", "address of the machine running the test servers")
	flag.Parse()

	// No device ID: these entries are for `task run` on this computer.
	st, err := store.Open(*data, nil)
	if err != nil {
		log.Fatal(err)
	}
	seeds := []store.Server{
		// The FTP image does not chroot: the share is at the user's home.
		{Name: "Dev FTP", Protocol: store.ProtoFTP, Host: *host, Port: 2121, Username: "test", SavePassword: true, Encoding: store.EncodingUTF8, InitialPath: "/ftp/test"},
		{Name: "Dev SFTP", Protocol: store.ProtoSFTP, Host: *host, Port: 2222, Username: "test", SavePassword: true, InitialPath: "/share"},
		{Name: "Dev SMB", Protocol: store.ProtoSMB, Host: *host, Port: 4445, Username: "test", SavePassword: true},
	}
	existing := map[string]store.Server{}
	for _, s := range st.Servers() {
		existing[s.Name] = s
	}
	for _, s := range seeds {
		if old, ok := existing[s.Name]; ok {
			s.ID = old.ID
		}
		if err := st.SetPassword(&s, "test"); err != nil {
			log.Fatal(err)
		}
		if _, err := st.Put(s); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%-9s %s://%s:%d\n", s.Name, s.Protocol, s.Host, s.Port)
	}
}
