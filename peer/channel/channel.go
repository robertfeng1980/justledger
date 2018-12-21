/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package channel

import (
	"strings"
	"time"

	"justledger/common/flogging"
	"justledger/msp"
	"justledger/peer/common"
	cb "justledger/protos/common"
	pb "justledger/protos/peer"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var logger = flogging.MustGetLogger("channelCmd")

const (
	EndorserRequired       bool = true
	EndorserNotRequired    bool = false
	OrdererRequired        bool = true
	OrdererNotRequired     bool = false
	PeerDeliverRequired    bool = true
	PeerDeliverNotRequired bool = false
)

var (
	// join related variables.
	genesisBlockPath string

	// create related variables
	channelID     string
	channelTxFile string
	outputBlock   string
	timeout       time.Duration
)

// Cmd returns the cobra command for Node
func Cmd(cf *ChannelCmdFactory) *cobra.Command {
	AddFlags(channelCmd)

	channelCmd.AddCommand(createCmd(cf))
	channelCmd.AddCommand(fetchCmd(cf))
	channelCmd.AddCommand(joinCmd(cf))
	channelCmd.AddCommand(listCmd(cf))
	channelCmd.AddCommand(updateCmd(cf))
	channelCmd.AddCommand(signconfigtxCmd(cf))
	channelCmd.AddCommand(getinfoCmd(cf))

	return channelCmd
}

// AddFlags adds flags for create and join
func AddFlags(cmd *cobra.Command) {
	common.AddOrdererFlags(cmd)
}

var flags *pflag.FlagSet

func init() {
	resetFlags()
}

// Explicitly define a method to facilitate tests
func resetFlags() {
	flags = &pflag.FlagSet{}

	flags.StringVarP(&genesisBlockPath, "blockpath", "b", common.UndefinedParamValue, "Path to file containing genesis block")
	flags.StringVarP(&channelID, "channelID", "c", common.UndefinedParamValue, "In case of a newChain command, the channel ID to create. It must be all lower case, less than 250 characters long and match the regular expression: [a-z][a-z0-9.-]*")
	flags.StringVarP(&channelTxFile, "file", "f", "", "Configuration transaction file generated by a tool such as configtxgen for submitting to orderer")
	flags.StringVarP(&outputBlock, "outputBlock", "", common.UndefinedParamValue, `The path to write the genesis block for the channel. (default ./<channelID>.block)`)
	flags.DurationVarP(&timeout, "timeout", "t", 5*time.Second, "Channel creation timeout")
}

func attachFlags(cmd *cobra.Command, names []string) {
	cmdFlags := cmd.Flags()
	for _, name := range names {
		if flag := flags.Lookup(name); flag != nil {
			cmdFlags.AddFlag(flag)
		} else {
			logger.Fatalf("Could not find flag '%s' to attach to commond '%s'", name, cmd.Name())
		}
	}
}

var channelCmd = &cobra.Command{
	Use:              "channel",
	Short:            "Operate a channel: create|fetch|join|list|update|signconfigtx|getinfo.",
	Long:             "Operate a channel: create|fetch|join|list|update|signconfigtx|getinfo.",
	PersistentPreRun: common.SetOrdererEnv,
}

type BroadcastClientFactory func() (common.BroadcastClient, error)

type deliverClientIntf interface {
	GetSpecifiedBlock(num uint64) (*cb.Block, error)
	GetOldestBlock() (*cb.Block, error)
	GetNewestBlock() (*cb.Block, error)
	Close() error
}

// ChannelCmdFactory holds the clients used by ChannelCmdFactory
type ChannelCmdFactory struct {
	EndorserClient   pb.EndorserClient
	Signer           msp.SigningIdentity
	BroadcastClient  common.BroadcastClient
	DeliverClient    deliverClientIntf
	BroadcastFactory BroadcastClientFactory
}

// InitCmdFactory init the ChannelCmdFactory with clients to endorser and orderer according to params
func InitCmdFactory(isEndorserRequired, isPeerDeliverRequired, isOrdererRequired bool) (*ChannelCmdFactory, error) {
	if isPeerDeliverRequired && isOrdererRequired {
		// this is likely a bug during development caused by adding a new cmd
		return nil, errors.New("ERROR - only a single deliver source is currently supported")
	}

	var err error
	cf := &ChannelCmdFactory{}

	cf.Signer, err = common.GetDefaultSignerFnc()
	if err != nil {
		return nil, errors.WithMessage(err, "error getting default signer")
	}

	cf.BroadcastFactory = func() (common.BroadcastClient, error) {
		return common.GetBroadcastClientFnc()
	}

	// for join and list, we need the endorser as well
	if isEndorserRequired {
		// creating an EndorserClient with these empty parameters will create a
		// connection using the values of "peer.address" and
		// "peer.tls.rootcert.file"
		cf.EndorserClient, err = common.GetEndorserClientFnc(common.UndefinedParamValue, common.UndefinedParamValue)
		if err != nil {
			return nil, errors.WithMessage(err, "error getting endorser client for channel")
		}
	}

	// for fetching blocks from a peer
	if isPeerDeliverRequired {
		cf.DeliverClient, err = common.NewDeliverClientForPeer(channelID)
		if err != nil {
			return nil, errors.WithMessage(err, "error getting deliver client for channel")
		}
	}

	// for create and fetch, we need the orderer as well
	if isOrdererRequired {
		if len(strings.Split(common.OrderingEndpoint, ":")) != 2 {
			return nil, errors.Errorf("ordering service endpoint %s is not valid or missing", common.OrderingEndpoint)
		}
		cf.DeliverClient, err = common.NewDeliverClientForOrderer(channelID)
		if err != nil {
			return nil, err
		}
	}
	logger.Infof("Endorser and orderer connections initialized")
	return cf, nil
}
