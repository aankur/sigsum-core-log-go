package main

// go run . | bash

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"

	"git.sigsum.org/sigsum-go/pkg/types"
)

var (
	shardHint  = flag.Uint64("shard_hint", 0, "shard hint (decimal)")
	message    = flag.String("message", "", "message (hex)")
	sk         = flag.String("sk", "", "secret key (hex)")
	domainHint = flag.String("domain_hint", "example.com", "domain hint (string)")
	base_url   = flag.String("base_url", "localhost:6965/testonly", "base url (string)")
)

func main() {
	flag.Parse()

	var privBuf [64]byte
	var priv ed25519.PrivateKey = ed25519.PrivateKey(privBuf[:])
	mustDecodeHex(*sk, priv[:])

	var p types.Hash
	if *message != "" {
		mustDecodeHex(*message, p[:])
	} else {
		mustPutRandom(p[:])
	}

	msg := types.Statement{
		ShardHint: *shardHint,
		Checksum:  *types.HashFn(p[:]),
	}
	sig := ed25519.Sign(priv, msg.ToBinary())

	fmt.Printf("echo \"shard_hint=%d\nmessage=%x\nsignature=%x\npublic_key=%x\ndomain_hint=%s\" | curl --data-binary @- %s/sigsum/v0/add-leaf\n",
		*shardHint,
		p[:],
		sig,
		priv.Public().(ed25519.PublicKey)[:],
		*domainHint,
		*base_url,
	)
}

func mustDecodeHex(s string, buf []byte) {
	b, err := hex.DecodeString(s)
	if err != nil {
		log.Fatal(err)
	}
	if len(b) != len(buf) {
		log.Fatal("bad flag: invalid buffer length")
	}
	copy(buf, b)
}

func mustPutRandom(buf []byte) {
	_, err := rand.Read(buf)
	if err != nil {
		log.Fatal(err)
	}
}
