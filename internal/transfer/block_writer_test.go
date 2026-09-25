package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"

	hadoop "github.com/colinmarc/hdfs/v2/internal/protocol/hadoop_common"
	hdfs "github.com/colinmarc/hdfs/v2/internal/protocol/hadoop_hdfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestPacketSize(t *testing.T) {
	bws := &blockWriteStream{}
	bws.buf.Write(make([]byte, outboundPacketSize*3))
	packet := bws.makePacket()

	assert.EqualValues(t, outboundPacketSize, len(packet.data))
}

func TestPacketSizeUndersize(t *testing.T) {
	bws := &blockWriteStream{}
	bws.buf.Write(make([]byte, outboundPacketSize-5))
	packet := bws.makePacket()

	assert.EqualValues(t, outboundPacketSize-5, len(packet.data))
}

func TestPacketSizeAlignment(t *testing.T) {
	bws := &blockWriteStream{}
	bws.buf.Write(make([]byte, outboundPacketSize*3))

	bws.offset = 5
	packet := bws.makePacket()

	assert.EqualValues(t, outboundChunkSize-5, len(packet.data))
}

func locatedBlockForWrite(storageIDs []string) *hdfs.LocatedBlockProto {
	locs := []*hdfs.DatanodeInfoProto{}
	types := []hdfs.StorageTypeProto{}
	for i := 0; i < 2; i++ {
		locs = append(locs, &hdfs.DatanodeInfoProto{Id: &hdfs.DatanodeIDProto{
			IpAddr:       proto.String("127.0.0.1"),
			HostName:     proto.String("localhost"),
			DatanodeUuid: proto.String("dn"),
			XferPort:     proto.Uint32(50010),
			InfoPort:     proto.Uint32(50075),
			IpcPort:      proto.Uint32(50020),
		}})
		types = append(types, hdfs.StorageTypeProto_DISK)
	}
	return &hdfs.LocatedBlockProto{
		B: &hdfs.ExtendedBlockProto{
			PoolId:          proto.String("bp"),
			BlockId:         proto.Uint64(1),
			GenerationStamp: proto.Uint64(1001),
		},
		Offset:       proto.Uint64(0),
		Locs:         locs,
		Corrupt:      proto.Bool(false),
		BlockToken:   &hadoop.TokenProto{Identifier: []byte{}, Password: []byte{}, Kind: proto.String(""), Service: proto.String("")},
		StorageTypes: types,
		StorageIDs:   storageIDs,
	}
}

// decodeWriteRequest parses the operation a BlockWriter sent: the two-byte
// protocol version, the op code and the length-prefixed request message.
func decodeWriteRequest(t *testing.T, raw []byte) *hdfs.OpWriteBlockProto {
	require.EqualValues(t, writeBlockOp, raw[2])
	op := &hdfs.OpWriteBlockProto{}
	require.NoError(t, readPrefixedMessage(bytes.NewReader(raw[3:]), op))
	return op
}

// The write request names the storage of every node in the pipeline when the
// NameNode supplied storage ids, and none when it did not.
func TestWriteRequestNamesPipelineStorages(t *testing.T) {
	bw := &BlockWriter{ClientName: "cl",
		Block: locatedBlockForWrite([]string{"storage-a", "storage-b"})}
	var buf bytes.Buffer
	require.NoError(t, bw.writeBlockWriteRequest(&buf))
	op := decodeWriteRequest(t, buf.Bytes())
	assert.Equal(t, "storage-a", op.GetStorageId())
	assert.Equal(t, []string{"storage-b"}, op.GetTargetStorageIds())

	bw = &BlockWriter{ClientName: "cl", Block: locatedBlockForWrite(nil)}
	buf.Reset()
	require.NoError(t, bw.writeBlockWriteRequest(&buf))
	op = decodeWriteRequest(t, buf.Bytes())
	assert.Nil(t, op.StorageId)
	assert.Empty(t, op.GetTargetStorageIds())
}

// serveWriteBlockResponse plays the datanode end of a pipe for one
// OP_WRITE_BLOCK: it reads the request and answers with resp.
func serveWriteBlockResponse(conn net.Conn, resp *hdfs.BlockOpResponseProto) {
	defer conn.Close()
	header := make([]byte, 3) // protocol version + op
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	if err := readPrefixedMessage(conn, &hdfs.OpWriteBlockProto{}); err != nil {
		return
	}
	b, err := makePrefixedMessage(resp)
	if err != nil {
		return
	}
	conn.Write(b)
}

func twoNodeBlockForWrite() *hdfs.LocatedBlockProto {
	block := locatedBlockForWrite(nil)
	block.Locs[0].Id.DatanodeUuid = proto.String("dn-first")
	block.Locs[1].Id = &hdfs.DatanodeIDProto{
		IpAddr:       proto.String("10.0.0.2"),
		HostName:     proto.String("dn2.example"),
		DatanodeUuid: proto.String("dn-second"),
		XferPort:     proto.Uint32(50011),
		InfoPort:     proto.Uint32(50075),
		IpcPort:      proto.Uint32(50020),
	}
	block.B.NumBytes = proto.Uint64(0)
	return block
}

func refusingDialer(resp *hdfs.BlockOpResponseProto) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		client, server := net.Pipe()
		go serveWriteBlockResponse(server, resp)
		return client, nil
	}
}

// A datanode that answers the write request with an error leaves the writer
// in the "setup failed" state, blaming the node the datanode named as the
// first bad link when it is in the pipeline, otherwise the connected node.
func TestWriteRefusedBlockRecordsFailedDatanode(t *testing.T) {
	bw := &BlockWriter{ClientName: "cl", Block: twoNodeBlockForWrite(), BlockSize: 1 << 20,
		DialFunc: refusingDialer(&hdfs.BlockOpResponseProto{
			Status:       hdfs.Status_ERROR.Enum(),
			Message:      proto.String("No such file or directory"),
			FirstBadLink: proto.String("10.0.0.2:50011"),
		})}
	n, err := bw.Write([]byte("hello"))
	assert.Equal(t, 0, n)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ERROR")
	assert.True(t, bw.SetupFailed())
	assert.Equal(t, "dn-second", bw.FailedDatanode().GetId().GetDatanodeUuid())
	assert.EqualValues(t, 0, bw.Block.GetB().GetNumBytes())

	bw = &BlockWriter{ClientName: "cl", Block: twoNodeBlockForWrite(), BlockSize: 1 << 20,
		DialFunc: refusingDialer(&hdfs.BlockOpResponseProto{Status: hdfs.Status_ERROR.Enum(), Message: proto.String("refused")})}
	_, err = bw.Write([]byte("hello"))
	require.Error(t, err)
	assert.True(t, bw.SetupFailed())
	assert.Equal(t, "dn-first", bw.FailedDatanode().GetId().GetDatanodeUuid())

	// A firstBadLink naming a node outside the pipeline falls back to the
	// connected node.
	bw = &BlockWriter{ClientName: "cl", Block: twoNodeBlockForWrite(), BlockSize: 1 << 20,
		DialFunc: refusingDialer(&hdfs.BlockOpResponseProto{
			Status: hdfs.Status_ERROR.Enum(), FirstBadLink: proto.String("192.0.2.9:1")})}
	_, err = bw.Write([]byte("hello"))
	require.Error(t, err)
	assert.Equal(t, "dn-first", bw.FailedDatanode().GetId().GetDatanodeUuid())
}

// A connection failure to the first datanode is a setup failure blamed on
// that node.
func TestWriteDialFailureRecordsFailedDatanode(t *testing.T) {
	dialErr := errors.New("connection refused")
	bw := &BlockWriter{ClientName: "cl", Block: twoNodeBlockForWrite(), BlockSize: 1 << 20,
		DialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, dialErr
		}}
	n, err := bw.Write([]byte("hello"))
	assert.Equal(t, 0, n)
	assert.Equal(t, dialErr, err)
	assert.True(t, bw.SetupFailed())
	assert.Equal(t, "dn-first", bw.FailedDatanode().GetId().GetDatanodeUuid())
}

// Once the datanode accepts the block the setup-failure state is clear.
func TestWriteAcceptedBlockClearsSetupFailure(t *testing.T) {
	bw := &BlockWriter{ClientName: "cl", Block: twoNodeBlockForWrite(), BlockSize: 1 << 20,
		DialFunc: refusingDialer(&hdfs.BlockOpResponseProto{Status: hdfs.Status_SUCCESS.Enum(), FirstBadLink: proto.String("")})}
	n, err := bw.Write([]byte("hello")) // buffered, no packet is sent yet
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.False(t, bw.SetupFailed())
	assert.Nil(t, bw.FailedDatanode())
}
