package hdfs

import (
	"errors"
	"os"
	"strings"
	"time"

	hdfs "github.com/colinmarc/hdfs/v2/internal/protocol/hadoop_hdfs"
	"google.golang.org/protobuf/proto"
)

// Truncate truncates the file specified by name to the given size, and returns
// the status any error encountered. The returned status will false in the case
// of any error or, if the error is nil, if HDFS indicated that the operation
// will be performed asynchronously and is not yet complete.
//
// On HopsFS clusters with async cloud upload enabled, the NameNode rejects a
// truncate whose target block is still uploading to the object store by
// throwing NotReplicatedYetException. This is retried with exponential
// backoff (~51s total budget), matching the retry shape used in the append
// and addBlock paths.
func (c *Client) Truncate(name string, size int64) (bool, error) {
	req := &hdfs.TruncateRequestProto{
		Src:        proto.String(name),
		NewLength:  proto.Uint64(uint64(size)),
		ClientName: proto.String(c.namenode.ClientName),
	}
	resp := &hdfs.TruncateResponseProto{}

	var err error
	initDelay := time.Duration(100)
	for i := 0; i < 9; i++ { // 1 min max
		err = c.namenode.Execute("truncate", req, resp)
		if err != nil && strings.Contains(err.Error(), "NotReplicatedYetException") {
			time.Sleep(initDelay * time.Millisecond)
			initDelay *= 2
		} else {
			break
		}
	}
	if err != nil {
		return false, &os.PathError{"truncate", name, interpretException(err)}
	} else if resp.Result == nil {
		return false, &os.PathError{"truncate", name, errors.New("unexpected empty response")}
	}

	return resp.GetResult(), nil
}
