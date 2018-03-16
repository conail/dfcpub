package client_test

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/NVIDIA/dfcpub/dfc"
	"github.com/NVIDIA/dfcpub/pkg/client"
	"github.com/NVIDIA/dfcpub/pkg/client/readers"
	"github.com/OneOfOne/xxhash"
)

var server *httptest.Server

func TestPutFile(t *testing.T) {
	err := putFile(1024, true /* withHash */)
	if err != nil {
		t.Fatal("Put file failed", err)
	}
}

func TestPutSG(t *testing.T) {
	size := 10
	sgl := dfc.NewSGLIO(uint64(size))
	defer sgl.Free()
	err := putSG(sgl, int64(size), false /* withHash */)
	if err != nil {
		t.Fatal(err)
	}
}

func putFile(size int64, withHash bool) error {
	fn := "dfc-client-test-" + client.FastRandomFilename(rand.New(rand.NewSource(time.Now().UnixNano())), 32)
	dir := "/tmp"
	r, err := readers.NewFileReader(dir, fn, size, withHash)
	if err != nil {
		return err
	}

	err = client.Put(server.URL, r, "bucket", "key", true /* silent */)

	r.Close()
	os.Remove(dir + "/" + fn)
	return err
}

func putInMem(size int64, withHash bool) error {
	r, err := readers.NewInMemReader(size, withHash)
	if err != nil {
		return err
	}
	defer r.Close()

	return client.Put(server.URL, r, "bucket", "key", true /* silent */)
}

func putRand(size int64, withHash bool) error {
	r, err := readers.NewRandReader(size, withHash)
	if err != nil {
		return err
	}
	defer r.Close()

	return client.Put(server.URL, r, "bucket", "key", true /* silent */)
}

func putSG(sgl *dfc.SGLIO, size int64, withHash bool) error {
	sgl.Reset()
	r, err := readers.NewSGReader(sgl, size, true /* withHash */)
	if err != nil {
		return err
	}
	defer r.Close()

	return client.Put(server.URL, r, "bucket", "key", true /* silent */)
}

func BenchmarkPutFileWithHash1M(b *testing.B) {
	for i := 0; i < b.N; i++ {
		err := putFile(1024*1024, true /* withHash */)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutInMemWithHash1M(b *testing.B) {
	for i := 0; i < b.N; i++ {
		err := putInMem(1024*1024, true /* withHash */)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutRandWithHash1M(b *testing.B) {
	for i := 0; i < b.N; i++ {
		err := putRand(1024*1024, true /* withHash */)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutSGWithHash1M(b *testing.B) {
	sgl := dfc.NewSGLIO(1024 * 1024)
	defer sgl.Free()

	for i := 0; i < b.N; i++ {
		err := putSG(sgl, 1024*1024, true /* withHash */)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutFileNoHash1M(b *testing.B) {
	for i := 0; i < b.N; i++ {
		err := putFile(1024*1024, false /* withHash */)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutInMemNoHash1M(b *testing.B) {
	for i := 0; i < b.N; i++ {
		err := putInMem(1024*1024, false /* withHash */)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutRandNoHash1M(b *testing.B) {
	for i := 0; i < b.N; i++ {
		err := putRand(1024*1024, false /* withHash */)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutSGNoHash1M(b *testing.B) {
	sgl := dfc.NewSGLIO(1024 * 1024)
	defer sgl.Free()

	for i := 0; i < b.N; i++ {
		err := putSG(sgl, 1024*1024, false /* withHash */)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutFileWithHash1MParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := putFile(1024*1024, true /* withHash */)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkPutInMemWithHash1MParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := putInMem(1024*1024, true /* withHash */)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkPutRandWithHash1MParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := putRand(1024*1024, true /* withHash */)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkPutSGWithHash1MParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		sgl := dfc.NewSGLIO(1024 * 1024)
		defer sgl.Free()

		for pb.Next() {
			err := putSG(sgl, 1024*1024, true /* withHash */)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestMain(m *testing.M) {
	verifyHash := flag.Bool("verifyhash", true, "True if verify hash when a packet is received")
	flag.Parse()

	// HTTP server; verifies xxhash (if configured)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			io.Copy(ioutil.Discard, bufio.NewReader(r.Body))
			r.Body.Close()
		}()

		errf := func(s int, msg string) {
			w.WriteHeader(s)
			w.Write([]byte(msg))
		}

		if *verifyHash == false {
			return
		}

		// Verify hash
		t := r.Header.Get(dfc.HeaderDfcChecksumType)
		if t != dfc.ChecksumXXHash {
			errf(http.StatusBadRequest, fmt.Sprintf("Do not know how to handle hash type %s", t))
			return
		}

		hasher := xxhash.New64()
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			errf(http.StatusBadRequest, fmt.Sprintf("Server failed to read, error %v", err))
			return
		}

		hasher.Write(data)
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(hasher.Sum64()))
		hash := hex.EncodeToString(b)
		v := r.Header.Get(dfc.HeaderDfcChecksumVal)
		if hash != v {
			errf(http.StatusNotAcceptable, fmt.Sprintf("Hash mismatch expected = %s, actual = %s", v, hash))
		}
	}))
	defer server.Close()

	os.Exit(m.Run())
}
