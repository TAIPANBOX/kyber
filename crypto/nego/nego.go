// Package nego implements cryptographic negotiation
// and secret entrypoint finding.
package nego

import (
	"fmt"
	"sort"
	"errors"
	"crypto/cipher"
	"encoding/binary"
	"dissent/crypto"
)


type Entry struct {
	Suite crypto.Suite	// Ciphersuite this public key is drawn from
	PubKey crypto.Point	// Public key of this entrypoint's owner
	Data []byte		// Entrypoint data decryptable by owner
}


// Writer produces a cryptographic negotiation header,
// which conceals a variable number of "entrypoints"
// within a variable-length binary blob of random-looking bits.
// Each entrypoint hidden in the blob is discoverable and usable only
// by the owner of a particular public key.
// Different public keys may be drawn from different ciphersuites,
// in any combination, without coordination between the ciphersuites.
//
// Each entrypoint contains a short fixed-length blob of encrypted data,
// which the owner of the entrypoint can decrypt and use
// to obtain keys and pointers to the "real" content.
// This "real" content is typically located after the negotiation header
// and encrypted with a symmetric key included in the entrypoint data,
// which can be (but doesn't have to be) shared by many or all entrypoints.
//
type Writer struct {
	layout
}


// A ciphersuite used in a negotiation header.
type suiteKey struct {

	// Ephemeral Diffie-Hellman key for all key-holders using this suite.
	// Should have a uniform representation, e.g., an Elligator point.
	dhpri crypto.Secret
	dhpub crypto.Point
	dhrep []byte
}

/*
func (s *suiteKey) fresh(suite crypto.Suite) {
	dhpri := entry.suite.Secret().Pick(rand)
	dhpub := entry.Suite.Point().Mul(nil, dhpri)
	dhrep := dhpub.UniformEncode()
	suites[suite] = suite{dhpri,dhpub}
}
*/

type suiteInfo struct {
	ste crypto.Suite		// ciphersuite
	tag []uint32			// per-position pseudorandom tag
	pos []int			// alternative point positions
	plen int			// length of each point in bytes
	max int				// limit of highest point field

	// layout info
	nodes []*node			// layout node for reserved positions
	lev int				// layout-chosen level for this suite
}

// Determine all the alternative DH point positions for a ciphersuite.
func (si *suiteInfo) init(ste crypto.Suite, nlevels int) {
	si.ste = ste
	si.tag = make([]uint32, nlevels)
	si.pos = make([]int, nlevels)
	si.plen = ste.Point().(crypto.Hiding).HideLen()	// XXX

	// Create a pseudo-random stream from which to pick positions
	str := fmt.Sprintf("NegoCipherSuite:%s", ste.String())
	rand := crypto.HashStream(ste, []byte(str), nil)

	// Alternative 0 is always at position 0, so start with level 1.
	levofs := 0			// starting offset for current level
	fmt.Printf("Suite %s positions:\n", ste.String())
	for i := 0; i < nlevels; i++ {

		// Pick a random position within this level
		var buf [4]byte
		rand.XORKeyStream(buf[:],buf[:])
		levlen := 1 << uint(i)	// # alt positions at this level
		levmask := levlen - 1	// alternative index mask
		si.tag[i] = binary.BigEndian.Uint32(buf[:])
		levidx := int(si.tag[i]) & levmask
		si.pos[i] = levofs + levidx * si.plen

		fmt.Printf("%d: idx %d/%d pos %d\n",
				i, levidx, levlen, si.pos[i])

		levofs += levlen * si.plen	// next level table offset
	}

	// Limit of highest point field
	si.max = si.pos[nlevels-1] + si.plen

	si.nodes = make([]*node, nlevels)
}

// Try to reserve a space for level i of this ciphersuite in the layout.
func (si *suiteInfo) layout(w *Writer, i int) bool {
	var n node
	lo := si.pos[i]			// compute byte extent
	hi := lo + si.plen
	n.init(si.ste, i, lo, hi, si.tag[i])	// create suitable node
	fmt.Printf("try insert %s:%d at %d-%d\n", si.ste.String(), i, lo, hi)
	if !w.layout.insert(&n) {
		return false
	}
	si.nodes[i] = &n
	return true
}


type suites struct {
	s []suiteInfo
}

func (s *suites) Len() int {
	return len(s.s)
}
func (s *suites) Less(i,j int) bool {
	return s.s[i].max < s.s[j].max
}
func (s *suites) Swap(i,j int) {
	s.s[i],s.s[j] = s.s[j],s.s[i]
}


// Initialize a Writer to produce one or more negotiation header
// containing a specified set of entrypoints,
// whose owners' public keys are drawn from a given set of ciphersuites.
//
// The caller must provide a map 'suiteLevel' with one key per ciphersuite,
// whose value is the maximum "level" in the header 
// at which the ciphersuite's ephemeral Diffie-Hellman Point may be encoded.
// This maximum level must be standardized for each ciphersuite,
// and should be log2(maxsuites), where maxsuites is the maximum number
// of unique ciphersuites that are likely to exist when this suite is defined.
//
// All entrypoints will carry a payload entryLen bytes long,
// although the content of these payloads need not be specified yet.
// This function lays out the entrypoints in the negotiation header,
// and returns the total size of the negotiation headers
// that will be produced from this layout.
//
// After this initialization and layout computation,
// multiple independent negotiation headers with varying entrypoint data
// may be produced more efficiently via Write().
//
func (w *Writer) Init(suiteLevel map[crypto.Suite]int,
			entryLen int, entrypoints []Entry,
			rand cipher.Stream) (int,error) {

	w.layout.init()

	// Determine the set of ciphersuites in use.
/*
	suites := make(map[crypto.Suite]struct{})
	for i := range(entrypoints) {
		entry := entrypoints[i]
		if _,ok := suites[suite]; !ok {
			// First time we've seen this ciphersuite.
			suites[suite] = struct{}{}
		}
	}
*/

	// Compute the alternative DH point positions for each ciphersuite,
	// and the maximum byte offset for each.
	stes := suites{}
	stes.s = make([]suiteInfo, 0, len(suiteLevel))
	for suite,nlevels := range suiteLevel {
		si := suiteInfo{}
		si.init(suite,nlevels)
		stes.s = append(stes.s, si)
	}
	nsuites := len(stes.s)

	// Sort the ciphersuites in order of max position,
	// to give ciphersuites with most restrictive positioning
	// "first dibs" on the lowest positions.
	sort.Sort(&stes)

	// Layout each of the ciphersuites.
	hdrlen := 0
	for i := 0; i < nsuites; i++ {
		s := &stes.s[i]
		fmt.Printf("max %d: %s\n", s.max, s.ste.String())

		// Find the lowest level that isn't shadowed by another suite,
		// ensuring that our point won't be corrupted when the points
		// for later (higher) suites get computed and filled in.
		j := len(s.pos)-1
		if !s.layout(w,j) {
			return 0,errors.New("failed to find viable position for ciphersuite "+s.ste.String())
		}
		for ; j > 0; j-- {
			if !s.layout(w,j-1) {	// is position j-1 free too?
				break		// no, stop at level j
			}
		}
		s.lev = j
		lim := s.pos[j] + s.plen
		if lim > hdrlen {
			hdrlen = lim
		}
		fmt.Printf("levels %d-%d\n", j, len(s.pos)-1)
	}
	fmt.Printf("hdrlen %d\n", hdrlen)

	fmt.Println("intermediate point layout:")
	w.layout.dump();

	// Now we can go back and unreserve all but the point position
	// for the picked level for each ciphersuite.
	for i := 0; i < nsuites; i++ {
		s := &stes.s[i]
		nlevels := len(s.pos)
		for j := s.lev+1; j < nlevels; j++ {
			w.remove(s.nodes[j])
			s.nodes[j] = nil
		}
	}

	fmt.Println("ciphersuite point layout:")
	w.layout.dump();

	return hdrlen,nil
}


// 
//func (w *Writer) Write(entryData map[Entry][]byte, suffix []byte)
