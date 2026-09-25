package hdfs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	hadoop "github.com/colinmarc/hdfs/v2/internal/protocol/hadoop_common"
	hdfs "github.com/colinmarc/hdfs/v2/internal/protocol/hadoop_hdfs"
	"github.com/colinmarc/hdfs/v2/internal/rpc"
	"github.com/colinmarc/hdfs/v2/internal/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// These tests drive FileWriter against a fake namenode and fake datanodes
// (served over in-memory pipes), so they need no cluster. They cover the
// abandonBlock + excludeNodes retry that FileWriter performs when a datanode
// refuses to open a block, mirroring DataStreamer.nextBlockOutputStream.

// fakeNamenode hands out one single-replica block per addBlock, on the
// datanode ports given in order, and records every RPC.
type fakeNamenode struct {
	mu          sync.Mutex
	ports       []uint32
	nextBlockID uint64

	addBlockReqs []*hdfs.AddBlockRequestProto
	abandoned    []uint64
	completed    []*hdfs.CompleteRequestProto
}

func (nn *fakeNamenode) Execute(method string, req proto.Message, resp proto.Message) error {
	nn.mu.Lock()
	defer nn.mu.Unlock()

	switch method {
	case "addBlock":
		nn.addBlockReqs = append(nn.addBlockReqs, proto.Clone(req).(*hdfs.AddBlockRequestProto))
		if len(nn.ports) == 0 {
			return errors.New("fake namenode: no datanodes left")
		}
		port := nn.ports[0]
		nn.ports = nn.ports[1:]
		nn.nextBlockID++
		resp.(*hdfs.AddBlockResponseProto).Block = fakeLocatedBlock(nn.nextBlockID, port)
	case "abandonBlock":
		nn.abandoned = append(nn.abandoned, req.(*hdfs.AbandonBlockRequestProto).GetB().GetBlockId())
	case "complete":
		nn.completed = append(nn.completed, proto.Clone(req).(*hdfs.CompleteRequestProto))
		resp.(*hdfs.CompleteResponseProto).Result = proto.Bool(true)
	default:
		return fmt.Errorf("fake namenode: unexpected rpc %s", method)
	}
	return nil
}

func fakeDatanodeUUID(port uint32) string { return fmt.Sprintf("dn-%d", port) }

func fakeLocatedBlock(id uint64, port uint32) *hdfs.LocatedBlockProto {
	return &hdfs.LocatedBlockProto{
		B: &hdfs.ExtendedBlockProto{
			PoolId:          proto.String("bp"),
			BlockId:         proto.Uint64(id),
			GenerationStamp: proto.Uint64(1000 + id),
			NumBytes:        proto.Uint64(0),
		},
		Offset: proto.Uint64(0),
		Locs: []*hdfs.DatanodeInfoProto{{Id: &hdfs.DatanodeIDProto{
			IpAddr:       proto.String("127.0.0.1"),
			HostName:     proto.String("localhost"),
			DatanodeUuid: proto.String(fakeDatanodeUUID(port)),
			XferPort:     proto.Uint32(port),
			InfoPort:     proto.Uint32(port + 1),
			IpcPort:      proto.Uint32(port + 2),
		}}},
		Corrupt:      proto.Bool(false),
		BlockToken:   &hadoop.TokenProto{Identifier: []byte{}, Password: []byte{}, Kind: proto.String(""), Service: proto.String("")},
		StorageTypes: []hdfs.StorageTypeProto{hdfs.StorageTypeProto_DISK},
		StorageIDs:   []string{fmt.Sprintf("storage-%d", port)},
	}
}

// fakeDatanodes serves OP_WRITE_BLOCK over net.Pipe. Datanodes on ports in
// `refusing` answer the request with Status_ERROR (as a DN whose rbw
// directory is gone does); the others accept and ack every packet.
type fakeDatanodes struct {
	mu       sync.Mutex
	refusing map[uint32]bool
	received map[uint32]*bytes.Buffer
	dialed   []string
}

func newFakeDatanodes(refusing ...uint32) *fakeDatanodes {
	d := &fakeDatanodes{refusing: map[uint32]bool{}, received: map[uint32]*bytes.Buffer{}}
	for _, p := range refusing {
		d.refusing[p] = true
	}
	return d
}

func (d *fakeDatanodes) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var port uint32
	if _, err := fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &port); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	refuse := d.refusing[port]
	d.mu.Unlock()

	client, server := net.Pipe()
	go d.serve(server, port, refuse)
	return client, nil
}

func writePrefixed(w io.Writer, msg proto.Message) error {
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	prefix := make([]byte, binary.MaxVarintLen32)
	n := binary.PutUvarint(prefix, uint64(len(b)))
	_, err = w.Write(append(prefix[:n], b...))
	return err
}

// readWriteBlockRequest reads the OP_WRITE_BLOCK request a BlockWriter sends
// when it opens a block: protocol version, op code and the length-prefixed
// OpWriteBlockProto.
func readWriteBlockRequest(r *bufio.Reader) (*hdfs.OpWriteBlockProto, error) {
	if _, err := io.ReadFull(r, make([]byte, 3)); err != nil { // version + op
		return nil, err
	}
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	reqBytes := make([]byte, n)
	if _, err := io.ReadFull(r, reqBytes); err != nil {
		return nil, err
	}
	op := &hdfs.OpWriteBlockProto{}
	if err := proto.Unmarshal(reqBytes, op); err != nil {
		return nil, err
	}
	return op, nil
}

// refuseWriteBlock plays a datanode that cannot store the block: it reads the
// write request and answers Status_ERROR, as a datanode whose rbw directory
// is gone does ("java.io.IOException: No such file or directory" from
// BlockPoolSlice.createRbwFile), then closes the connection.
func refuseWriteBlock(conn net.Conn) {
	defer conn.Close()
	if _, err := readWriteBlockRequest(bufio.NewReader(conn)); err != nil {
		return
	}
	writePrefixed(conn, &hdfs.BlockOpResponseProto{
		Status:       hdfs.Status_ERROR.Enum(),
		FirstBadLink: proto.String(""),
		Message:      proto.String("java.io.IOException: No such file or directory"),
	})
}

func (d *fakeDatanodes) serve(conn net.Conn, port uint32, refuse bool) {
	if refuse {
		refuseWriteBlock(conn)
		return
	}

	defer conn.Close()
	r := bufio.NewReader(conn)
	if _, err := readWriteBlockRequest(r); err != nil {
		return
	}

	if err := writePrefixed(conn, &hdfs.BlockOpResponseProto{
		Status: hdfs.Status_SUCCESS.Enum(), FirstBadLink: proto.String("")}); err != nil {
		return
	}

	for {
		var packetLen uint32
		var headerLen uint16
		if err := binary.Read(r, binary.BigEndian, &packetLen); err != nil {
			return
		}
		if err := binary.Read(r, binary.BigEndian, &headerLen); err != nil {
			return
		}
		headerBytes := make([]byte, headerLen)
		if _, err := io.ReadFull(r, headerBytes); err != nil {
			return
		}
		header := &hdfs.PacketHeaderProto{}
		if err := proto.Unmarshal(headerBytes, header); err != nil {
			return
		}
		body := make([]byte, packetLen-4) // checksums + data
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}

		if header.GetSeqno() != -1 { // not a heartbeat
			d.mu.Lock()
			if d.received[port] == nil {
				d.received[port] = &bytes.Buffer{}
			}
			d.received[port].Write(body[len(body)-int(header.GetDataLen()):])
			d.mu.Unlock()
		}

		if err := writePrefixed(conn, &hdfs.PipelineAckProto{
			Seqno: proto.Int64(header.GetSeqno()),
			Reply: []hdfs.Status{hdfs.Status_SUCCESS},
		}); err != nil {
			return
		}
		if header.GetLastPacketInBlock() {
			return
		}
	}
}

func (d *fakeDatanodes) data(port uint32) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.received[port] == nil {
		return nil
	}
	return d.received[port].Bytes()
}

func newRetryTestWriter(nn *fakeNamenode, dns *fakeDatanodes, retries int, blockSize int64, storeInDB bool) *FileWriter {
	c := &Client{
		namenode: &rpc.NamenodeConnection{ClientName: "gohdfs-test"},
		options:  ClientOptions{DatanodeDialFunc: dns.dial, BlockWriteRetries: retries},
		defaults: &hdfs.FsServerDefaultsProto{},
	}
	return &FileWriter{
		client:          c,
		nn:              nn,
		name:            "/retry/test",
		replication:     1,
		blockSize:       blockSize,
		storeInDB:       storeInDB,
		smallFileBuffer: []byte{},
	}
}

func excludedUUIDs(req *hdfs.AddBlockRequestProto) []string {
	var out []string
	for _, n := range req.GetExcludeNodes() {
		out = append(out, n.GetId().GetDatanodeUuid())
	}
	return out
}

// A datanode that refuses the block is excluded, the block is abandoned, and
// the same bytes land on the datanode of the replacement block. complete is
// sent with the replacement block, never with the abandoned one.
func TestWriteRetriesRefusedBlockOnAnotherDatanode(t *testing.T) {
	nn := &fakeNamenode{ports: []uint32{50010, 50020}}
	dns := newFakeDatanodes(50010)
	w := newRetryTestWriter(nn, dns, 0, 1<<20, false)

	payload := bytes.Repeat([]byte("hopsfs"), 500)
	n, err := w.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, len(payload), n)
	require.NoError(t, w.Close())

	require.Len(t, nn.addBlockReqs, 2)
	assert.Empty(t, nn.addBlockReqs[0].GetExcludeNodes())
	assert.Equal(t, []string{fakeDatanodeUUID(50010)}, excludedUUIDs(nn.addBlockReqs[1]))
	assert.Nil(t, nn.addBlockReqs[1].GetPrevious(), "replacement block is allocated after the same previous block")
	assert.Equal(t, []uint64{1}, nn.abandoned)

	require.Len(t, nn.completed, 1)
	assert.EqualValues(t, 2, nn.completed[0].GetLast().GetBlockId())
	assert.EqualValues(t, len(payload), nn.completed[0].GetLast().GetNumBytes())

	assert.Nil(t, dns.data(50010))
	assert.Equal(t, payload, dns.data(50020))
}

// When every allocation is refused the writer gives up after 1 + retries
// attempts, each refused block having been abandoned. Close returns the same
// error and never calls complete; the writer stays failed.
func TestWriteGivesUpAfterBlockWriteRetries(t *testing.T) {
	nn := &fakeNamenode{ports: []uint32{50010, 50020, 50030, 50040, 50050, 50060}}
	dns := newFakeDatanodes(50010, 50020, 50030, 50040, 50050, 50060)
	w := newRetryTestWriter(nn, dns, 0, 1<<20, false) // default: 3 retries

	n, err := w.Write([]byte("hello"))
	assert.Equal(t, 0, n)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unable to create new block after 4 attempts")
	assert.Contains(t, err.Error(), "No such file or directory")

	assert.Len(t, nn.addBlockReqs, 4)
	assert.Equal(t, []uint64{1, 2, 3, 4}, nn.abandoned)
	assert.Equal(t, []string{
		fakeDatanodeUUID(50010), fakeDatanodeUUID(50020), fakeDatanodeUUID(50030),
	}, excludedUUIDs(nn.addBlockReqs[3]))

	closeErr := w.Close()
	assert.Equal(t, err, closeErr)
	assert.Empty(t, nn.completed, "complete must never be sent for a block with no data")

	_, again := w.Write([]byte("more"))
	assert.Equal(t, err, again)
}

// The retry count is configurable, and a negative value disables the retry
// (and the abandon) altogether, restoring the plain error.
func TestWriteBlockWriteRetriesOption(t *testing.T) {
	nn := &fakeNamenode{ports: []uint32{50010, 50020, 50030}}
	dns := newFakeDatanodes(50010, 50020, 50030)
	w := newRetryTestWriter(nn, dns, 1, 1<<20, false)
	_, err := w.Write([]byte("hello"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "after 2 attempts")
	assert.Equal(t, []uint64{1, 2}, nn.abandoned)

	nn = &fakeNamenode{ports: []uint32{50010, 50020}}
	dns = newFakeDatanodes(50010)
	w = newRetryTestWriter(nn, dns, -1, 1<<20, false)
	_, err = w.Write([]byte("hello"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write failed: ERROR")
	assert.Len(t, nn.addBlockReqs, 1)
	assert.Empty(t, nn.abandoned)
	assert.Equal(t, err, w.Close())
	assert.Empty(t, nn.completed)
}

// A file that outgrows the small-file (DB) buffer and then cannot be written
// to any datanode must fail Close instead of completing the file with a dead
// block, or with no block at all (a 0-byte file).
func TestSmallFileOverflowWriteFailureFailsClose(t *testing.T) {
	nn := &fakeNamenode{ports: []uint32{50010, 50020, 50030, 50040}}
	dns := newFakeDatanodes(50010, 50020, 50030, 50040)
	w := newRetryTestWriter(nn, dns, 0, 1<<20, true)

	_, err := w.Write(make([]byte, MaxSmallFileSize))
	require.NoError(t, err) // buffered for the DB
	_, err = w.Write([]byte("one more byte"))
	require.Error(t, err)

	closeErr := w.Close()
	assert.Equal(t, err, closeErr)
	assert.Empty(t, nn.completed)
	assert.Equal(t, []uint64{1, 2, 3, 4}, nn.abandoned)

	// addBlock itself failing in the overflow branch must not complete the
	// file either.
	nn = &fakeNamenode{}
	w = newRetryTestWriter(nn, newFakeDatanodes(), 0, 1<<20, true)
	_, err = w.Write(make([]byte, MaxSmallFileSize+1))
	require.Error(t, err)
	assert.Equal(t, err, w.Close())
	assert.Empty(t, nn.completed)
}

// A refused block in the middle of a multi-block file is re-allocated after
// the last good block, and the data of every block ends up where the
// namenode placed it.
func TestWriteRetriesRefusedMiddleBlock(t *testing.T) {
	const blockSize = 4096
	nn := &fakeNamenode{ports: []uint32{50010, 50020, 50030, 50040}}
	dns := newFakeDatanodes(50020)
	w := newRetryTestWriter(nn, dns, 0, blockSize, false)

	payload := make([]byte, 3*blockSize-100)
	for i := range payload {
		payload[i] = byte(i)
	}
	n, err := w.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, len(payload), n)
	require.NoError(t, w.Close())

	require.Len(t, nn.addBlockReqs, 4)
	assert.EqualValues(t, 1, nn.addBlockReqs[1].GetPrevious().GetBlockId())
	assert.EqualValues(t, 1, nn.addBlockReqs[2].GetPrevious().GetBlockId(), "retry is allocated after block 1, not after the abandoned block 2")
	assert.Equal(t, []string{fakeDatanodeUUID(50020)}, excludedUUIDs(nn.addBlockReqs[2]))
	assert.Equal(t, []string{fakeDatanodeUUID(50020)}, excludedUUIDs(nn.addBlockReqs[3]), "the exclusion sticks for later blocks of the file")
	assert.EqualValues(t, 3, nn.addBlockReqs[3].GetPrevious().GetBlockId())
	assert.Equal(t, []uint64{2}, nn.abandoned)

	require.Len(t, nn.completed, 1)
	assert.EqualValues(t, 4, nn.completed[0].GetLast().GetBlockId())

	got := append(append(append([]byte{}, dns.data(50010)...), dns.data(50030)...), dns.data(50040)...)
	assert.Equal(t, payload, got)
	assert.Nil(t, dns.data(50020))
}

// An appended block already holds data that a fresh block would not, so a
// refusal to reopen it is not retried on another datanode.
func TestAppendRefusedBlockIsNotRetried(t *testing.T) {
	nn := &fakeNamenode{ports: []uint32{50020}}
	dns := newFakeDatanodes(50010)
	w := newRetryTestWriter(nn, dns, 0, 1<<20, false)
	block := fakeLocatedBlock(7, 50010)
	block.B.NumBytes = proto.Uint64(100)
	w.pos = 100
	w.blockWriter = &transfer.BlockWriter{
		ClientName: "gohdfs-test",
		Block:      block,
		BlockSize:  1 << 20,
		Offset:     100,
		Append:     true,
		DialFunc:   dns.dial,
	}

	_, err := w.Write([]byte("tail"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write failed: ERROR")
	assert.Empty(t, nn.abandoned)
	assert.Empty(t, nn.addBlockReqs)
	assert.Equal(t, err, w.Close())
	assert.Empty(t, nn.completed)
}
