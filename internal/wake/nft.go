package wake

import (
	"bytes"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
)

const (
	// ListenPort is where the rules redirect connections to sleeping services.
	ListenPort = 15000
	// proxyMark is the SO_MARK of the node's own connections to a service. They
	// leave through the output hook and without it the rule would send them back
	// to the proxy while the target is in the set.
	proxyMark = 0x414e
)

// rulesScript is the nft -f - input that creates table inet anden_wake of D04,
// replacing what a previous agent of this node left. The empty table makes the
// delete valid when none exists yet.
func rulesScript() string {
	return fmt.Sprintf(`table inet anden_wake {}
delete table inet anden_wake
table inet anden_wake {
  set sleeping { type ipv4_addr . inet_service; }
  chain prerouting { type nat hook prerouting priority dstnat; ip daddr . tcp dport @sleeping redirect to :%[1]d; }
  chain output { type nat hook output priority dstnat; meta mark %#[2]x return; ip daddr . tcp dport @sleeping redirect to :%[1]d; }
}
`, ListenPort, proxyMark)
}

// elementScript is the transaction that adds a target to the set or removes it.
func elementScript(verb string, target netip.AddrPort) string {
	return fmt.Sprintf("%s element inet anden_wake sleeping { %s . %d }\n", verb, target.Addr(), target.Port())
}

// NFT applies transactions with nft -f -, one at a time.
type NFT struct {
	mu    sync.Mutex
	apply func(script string) error
}

func NewNFT() *NFT {
	return &NFT{apply: runNft}
}

// Setup creates the table of the node, with an empty set.
func (n *NFT) Setup() error { return n.run(rulesScript()) }

// Add puts target in the set. Adding what is already there is not an error.
func (n *NFT) Add(target netip.AddrPort) error { return n.run(elementScript("add", target)) }

// Delete takes target out of the set. It fails if target is not there.
func (n *NFT) Delete(target netip.AddrPort) error { return n.run(elementScript("delete", target)) }

func (n *NFT) run(script string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.apply(script)
}

func runNft(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft -f -: %w: %s", err, out.String())
	}
	return nil
}
