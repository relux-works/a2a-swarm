package devicelist

import (
	"crypto/elliptic"
	"math/big"
)

// AlgMatrixEd25519 is the only alg value with a verifier in M5n (O3.17).
const AlgMatrixEd25519 = "matrix-ed25519"

type algClass int

const (
	algVerifier algClass = iota + 1 // A verifier exists here: matrix-ed25519.
	algRuled                        // Rules in section 4, no messenger verifier: bip340, p256.
	algReserved                     // Registered so a list may name it; no verifier (O3.17).
)

// algEntry is one alg value of the section 3.6 registry: the length of its
// key in bytes, the highest ceiling a verifier admits for the value (O3.16)
// and, where section 3.1 states one, the validity rule of the key. A reserved
// value has its length checked and nothing more: its encoding is not agreed.
type algEntry struct {
	keyBytes int
	highest  int
	class    algClass
	valid    func([]byte) bool
}

var registry = map[string]algEntry{
	"bip340":          {32, 1, algRuled, validBIP340},
	"p256":            {33, 2, algRuled, validP256},
	AlgMatrixEd25519:  {32, 1, algVerifier, validEd25519},
	"fido2-es256":     {33, 2, algReserved, nil},
	"fido2-eddsa":     {32, 2, algReserved, nil},
	"p384":            {49, 1, algReserved, nil},
	"omemo-xeddsa":    {32, 1, algReserved, nil},
	"signal-xeddsa":   {32, 1, algReserved, nil},
	"openpgp-ed25519": {32, 1, algReserved, nil},
	"ssh-ed25519":     {32, 1, algReserved, nil},
	"ssh-sk-ecdsa":    {33, 2, algReserved, nil},
}

func mustHex(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("devicelist: bad constant")
	}
	return n
}

var (
	one     = big.NewInt(1)
	secpP   = mustHex("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f")
	edP     = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	edD     = mustHex("52036cee2b6ffe738cc740797779e89800700a4d4141d8ab75eb4dca135978a3")
	edSqrtM = new(big.Int).Exp(big.NewInt(2), new(big.Int).Rsh(new(big.Int).Sub(edP, one), 2), edP)
)

// hasSqrt reports whether c is a square modulo p, for a prime p = 3 mod 4.
func hasSqrt(c, p *big.Int) bool {
	e := new(big.Int).Rsh(new(big.Int).Add(p, one), 2)
	r := new(big.Int).Exp(c, e, p)
	r.Mul(r, r).Mod(r, p)
	return r.Cmp(new(big.Int).Mod(c, p)) == 0
}

// validBIP340 is BIP-340 lift_x: x below the field prime and x^3+7 a square.
func validBIP340(key []byte) bool {
	if len(key) != 32 {
		return false
	}
	x := new(big.Int).SetBytes(key)
	if x.Cmp(secpP) >= 0 {
		return false
	}
	c := new(big.Int).Exp(x, big.NewInt(3), secpP)
	c.Add(c, big.NewInt(7)).Mod(c, secpP)
	return hasSqrt(c, secpP)
}

// validP256 is the 33-byte compressed SEC1 point of section 3.1: prefix 02
// or 03, x below the field prime and on the curve. The identity has no
// compressed encoding of this length.
func validP256(key []byte) bool {
	if len(key) != 33 || (key[0] != 2 && key[0] != 3) {
		return false
	}
	params := elliptic.P256().Params()
	x := new(big.Int).SetBytes(key[1:])
	if x.Cmp(params.P) >= 0 {
		return false
	}
	c := new(big.Int).Exp(x, big.NewInt(3), params.P)
	c.Sub(c, new(big.Int).Mul(x, big.NewInt(3)))
	c.Add(c, params.B).Mod(c, params.P)
	return hasSqrt(c, params.P)
}

// validEd25519 is the matrix-ed25519 key rule of section 3.1: a canonical
// encoding (y below the field prime, no sign bit on x = 0) of a point on the
// curve that is not of small order, which is a point P with [8]P not the
// identity.
func validEd25519(key []byte) bool {
	if len(key) != 32 {
		return false
	}
	be := make([]byte, 32)
	for i, b := range key {
		be[31-i] = b
	}
	sign := uint(be[0] >> 7)
	be[0] &= 0x7f
	y := new(big.Int).SetBytes(be)
	if y.Cmp(edP) >= 0 {
		return false
	}
	yy := new(big.Int).Mul(y, y)
	yy.Mod(yy, edP)
	u := new(big.Int).Sub(yy, one)
	u.Mod(u, edP)
	v := new(big.Int).Mul(edD, yy)
	v.Add(v, one).Mod(v, edP)
	inverse := new(big.Int).ModInverse(v, edP)
	if inverse == nil {
		return false
	}
	xx := new(big.Int).Mul(u, inverse)
	xx.Mod(xx, edP)
	// p = 5 mod 8: the candidate root is xx^((p+3)/8), times sqrt(-1) if needed.
	e := new(big.Int).Rsh(new(big.Int).Add(edP, big.NewInt(3)), 3)
	x := new(big.Int).Exp(xx, e, edP)
	if !squareIs(x, xx) {
		x.Mul(x, edSqrtM).Mod(x, edP)
		if !squareIs(x, xx) {
			return false
		}
	}
	if x.Sign() == 0 && sign == 1 {
		return false
	}
	px, py := x, y
	for i := 0; i < 3; i++ {
		px, py = edDouble(px, py)
	}
	return !(px.Sign() == 0 && py.Cmp(one) == 0)
}

func squareIs(x, want *big.Int) bool {
	s := new(big.Int).Mul(x, x)
	s.Mod(s, edP)
	return s.Cmp(want) == 0
}

// edDouble doubles an affine point of edwards25519 with the complete addition
// law. A denominator without an inverse cannot occur for a point on the
// curve; it returns the identity, which the caller refuses.
func edDouble(x, y *big.Int) (*big.Int, *big.Int) {
	xy := new(big.Int).Mul(x, y)
	xy.Mod(xy, edP)
	t := new(big.Int).Mul(xy, xy)
	t.Mul(t, edD).Mod(t, edP)
	xn := new(big.Int).Lsh(xy, 1)
	yn := new(big.Int).Mul(y, y)
	yn.Add(yn, new(big.Int).Mul(x, x))
	xd := new(big.Int).ModInverse(new(big.Int).Mod(new(big.Int).Add(one, t), edP), edP)
	yd := new(big.Int).ModInverse(new(big.Int).Mod(new(big.Int).Sub(one, t), edP), edP)
	if xd == nil || yd == nil {
		return new(big.Int), big.NewInt(1)
	}
	xn.Mul(xn, xd).Mod(xn, edP)
	yn.Mul(yn, yd).Mod(yn, edP)
	return xn, yn
}
