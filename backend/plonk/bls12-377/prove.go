// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark DO NOT EDIT

package plonk

import (
	"crypto/sha256"
	"math/big"
	"runtime"
	"sync"
	"time"

	"github.com/consensys/gnark/backend/witness"

	"github.com/consensys/gnark-crypto/ecc/bls12-377/fr"

	curve "github.com/consensys/gnark-crypto/ecc/bls12-377"

	"github.com/consensys/gnark-crypto/ecc/bls12-377/fr/kzg"

	"github.com/consensys/gnark-crypto/ecc/bls12-377/fr/fft"

	"github.com/consensys/gnark-crypto/ecc/bls12-377/fr/iop"
	cs "github.com/consensys/gnark/constraint/bls12-377"

	"github.com/consensys/gnark-crypto/fiat-shamir"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/constraint/solver"
	"github.com/consensys/gnark/internal/utils"
	"github.com/consensys/gnark/logger"
)

type Proof struct {

	// Commitments to the solution vectors
	LRO [3]kzg.Digest

	// Commitment to Z, the permutation polynomial
	Z kzg.Digest

	// Commitments to h1, h2, h3 such that h = h1 + Xh2 + X**2h3 is the quotient polynomial
	H [3]kzg.Digest

	Bsb22Commitments []kzg.Digest

	// Batch opening proof of h1 + zeta*h2 + zeta**2h3, linearizedPolynomial, l, r, o, s1, s2, qCPrime
	BatchedProof kzg.BatchOpeningProof

	// Opening proof of Z at zeta*mu
	ZShiftedOpening kzg.OpeningProof
}

// Computing and verifying Bsb22 multi-commits explained in https://hackmd.io/x8KsadW3RRyX7YTCFJIkHg
func bsb22ComputeCommitmentHint(spr *cs.SparseR1CS, pk *ProvingKey, proof *Proof, cCommitments []*iop.Polynomial, res *fr.Element, commDepth int) solver.Hint {
	return func(_ *big.Int, ins, outs []*big.Int) error {
		commitmentInfo := spr.CommitmentInfo.(constraint.PlonkCommitments)[commDepth]
		committedValues := make([]fr.Element, pk.Domain[0].Cardinality)
		offset := spr.GetNbPublicVariables()
		for i := range ins {
			committedValues[offset+commitmentInfo.Committed[i]].SetBigInt(ins[i])
		}
		var (
			err     error
			hashRes []fr.Element
		)
		if _, err = committedValues[offset+commitmentInfo.CommitmentIndex].SetRandom(); err != nil { // Commitment injection constraint has qcp = 0. Safe to use for blinding.
			return err
		}
		if _, err = committedValues[offset+spr.GetNbConstraints()-1].SetRandom(); err != nil { // Last constraint has qcp = 0. Safe to use for blinding
			return err
		}
		pi2iop := iop.NewPolynomial(&committedValues, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})
		cCommitments[commDepth] = pi2iop.ShallowClone()
		cCommitments[commDepth].ToCanonical(&pk.Domain[0]).ToRegular()
		if proof.Bsb22Commitments[commDepth], err = kzg.Commit(cCommitments[commDepth].Coefficients(), pk.Kzg); err != nil {
			return err
		}
		if hashRes, err = fr.Hash(proof.Bsb22Commitments[commDepth].Marshal(), []byte("BSB22-Plonk"), 1); err != nil {
			return err
		}
		res.Set(&hashRes[0]) // TODO @Tabaie use CommitmentIndex for this; create a new variable CommitmentConstraintIndex for other uses
		res.BigInt(outs[0])

		return nil
	}
}

func Prove(spr *cs.SparseR1CS, pk *ProvingKey, fullWitness witness.Witness, opts ...backend.ProverOption) (*Proof, error) {

	log := logger.Logger().With().Str("curve", spr.CurveID().String()).Int("nbConstraints", spr.GetNbConstraints()).Str("backend", "plonk").Logger()

	opt, err := backend.NewProverConfig(opts...)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	// pick a hash function that will be used to derive the challenges
	hFunc := sha256.New()

	// create a transcript manager to apply Fiat Shamir
	fs := fiatshamir.NewTranscript(hFunc, "gamma", "beta", "alpha", "zeta")

	// result
	proof := &Proof{}

	commitmentInfo := spr.CommitmentInfo.(constraint.PlonkCommitments)
	commitmentVal := make([]fr.Element, len(commitmentInfo)) // TODO @Tabaie get rid of this
	cCommitments := make([]*iop.Polynomial, len(commitmentInfo))
	proof.Bsb22Commitments = make([]kzg.Digest, len(commitmentInfo))
	for i := range commitmentInfo {
		opt.SolverOpts = append(opt.SolverOpts, solver.OverrideHint(commitmentInfo[i].HintID,
			bsb22ComputeCommitmentHint(spr, pk, proof, cCommitments, &commitmentVal[i], i)))
	}

	if spr.GkrInfo.Is() {
		var gkrData cs.GkrSolvingData
		opt.SolverOpts = append(opt.SolverOpts,
			solver.OverrideHint(spr.GkrInfo.SolveHintID, cs.GkrSolveHint(spr.GkrInfo, &gkrData)),
			solver.OverrideHint(spr.GkrInfo.ProveHintID, cs.GkrProveHint(spr.GkrInfo.HashName, &gkrData)))
	}

	// query l, r, o in Lagrange basis, not blinded
	_solution, err := spr.Solve(fullWitness, opt.SolverOpts...)
	if err != nil {
		return nil, err
	}
	// TODO @gbotrel deal with that conversion lazily
	lcCommitments := make([]*iop.Polynomial, len(cCommitments))
	for i := range cCommitments {
		lcCommitments[i] = cCommitments[i].Clone(int(pk.Domain[1].Cardinality)).ToLagrangeCoset(&pk.Domain[1]) // lagrange coset form
	}
	solution := _solution.(*cs.SparseR1CSSolution)
	evaluationLDomainSmall := []fr.Element(solution.L)
	evaluationRDomainSmall := []fr.Element(solution.R)
	evaluationODomainSmall := []fr.Element(solution.O)

	lagReg := iop.Form{Basis: iop.Lagrange, Layout: iop.Regular}
	// l, r, o and blinded versions
	var (
		wliop,
		wriop,
		woiop,
		bwliop,
		bwriop,
		bwoiop *iop.Polynomial
	)
	var wgLRO sync.WaitGroup
	wgLRO.Add(3)
	go func() {
		// we keep in lagrange regular form since iop.BuildRatioCopyConstraint prefers it in this form.
		wliop = iop.NewPolynomial(&evaluationLDomainSmall, lagReg)
		// we set the underlying slice capacity to domain[1].Cardinality to minimize mem moves.
		bwliop = wliop.Clone(int(pk.Domain[1].Cardinality)).ToCanonical(&pk.Domain[0]).ToRegular().Blind(1)
		wgLRO.Done()
	}()
	go func() {
		wriop = iop.NewPolynomial(&evaluationRDomainSmall, lagReg)
		bwriop = wriop.Clone(int(pk.Domain[1].Cardinality)).ToCanonical(&pk.Domain[0]).ToRegular().Blind(1)
		wgLRO.Done()
	}()
	go func() {
		woiop = iop.NewPolynomial(&evaluationODomainSmall, lagReg)
		bwoiop = woiop.Clone(int(pk.Domain[1].Cardinality)).ToCanonical(&pk.Domain[0]).ToRegular().Blind(1)
		wgLRO.Done()
	}()

	fw, ok := fullWitness.Vector().(fr.Vector)
	if !ok {
		return nil, witness.ErrInvalidWitness
	}

	// start computing lcqk
	var lcqk *iop.Polynomial
	chLcqk := make(chan struct{}, 1)
	go func() {
		// compute qk in canonical basis, completed with the public inputs
		// We copy the coeffs of qk to pk is not mutated
		lqkcoef := pk.lQk.Coefficients()
		qkCompletedCanonical := make([]fr.Element, len(lqkcoef))
		copy(qkCompletedCanonical, fw[:len(spr.Public)])
		copy(qkCompletedCanonical[len(spr.Public):], lqkcoef[len(spr.Public):])
		for i := range commitmentInfo {
			qkCompletedCanonical[spr.GetNbPublicVariables()+commitmentInfo[i].CommitmentIndex] = commitmentVal[i]
		}
		pk.Domain[0].FFTInverse(qkCompletedCanonical, fft.DIF)
		fft.BitReverse(qkCompletedCanonical)

		canReg := iop.Form{Basis: iop.Canonical, Layout: iop.Regular}
		lcqk = iop.NewPolynomial(&qkCompletedCanonical, canReg)
		lcqk.ToLagrangeCoset(&pk.Domain[1])
		close(chLcqk)
	}()

	// The first challenge is derived using the public data: the commitments to the permutation,
	// the coefficients of the circuit, and the public inputs.
	// derive gamma from the Comm(blinded cl), Comm(blinded cr), Comm(blinded co)
	if err := bindPublicData(&fs, "gamma", *pk.Vk, fw[:len(spr.Public)], proof.Bsb22Commitments); err != nil {
		return nil, err
	}

	// wait for polys to be blinded
	wgLRO.Wait()
	if err := commitToLRO(bwliop.Coefficients(), bwriop.Coefficients(), bwoiop.Coefficients(), proof, pk.Kzg); err != nil {
		return nil, err
	}

	gamma, err := deriveRandomness(&fs, "gamma", &proof.LRO[0], &proof.LRO[1], &proof.LRO[2]) // TODO @Tabaie @ThomasPiellard add BSB commitment here?
	if err != nil {
		return nil, err
	}

	// Fiat Shamir this
	bbeta, err := fs.ComputeChallenge("beta")
	if err != nil {
		return nil, err
	}
	var beta fr.Element
	beta.SetBytes(bbeta)

	// l, r, o are already blinded
	wgLRO.Add(3)
	go func() {
		bwliop.ToLagrangeCoset(&pk.Domain[1])
		wgLRO.Done()
	}()
	go func() {
		bwriop.ToLagrangeCoset(&pk.Domain[1])
		wgLRO.Done()
	}()
	go func() {
		bwoiop.ToLagrangeCoset(&pk.Domain[1])
		wgLRO.Done()
	}()

	// compute the copy constraint's ratio
	// note that wliop, wriop and woiop are fft'ed (mutated) in the process.
	ziop, err := iop.BuildRatioCopyConstraint(
		[]*iop.Polynomial{
			wliop,
			wriop,
			woiop,
		},
		pk.trace.S,
		beta,
		gamma,
		iop.Form{Basis: iop.Canonical, Layout: iop.Regular},
		&pk.Domain[0],
	)
	if err != nil {
		return proof, err
	}

	// commit to the blinded version of z
	chZ := make(chan error, 1)
	var bwziop, bwsziop *iop.Polynomial
	var alpha fr.Element
	go func() {
		bwziop = ziop // iop.NewWrappedPolynomial(&ziop)
		bwziop.Blind(2)
		proof.Z, err = kzg.Commit(bwziop.Coefficients(), pk.Kzg, runtime.NumCPU()*2)
		if err != nil {
			chZ <- err
		}

		// derive alpha from the Comm(l), Comm(r), Comm(o), Com(Z)
		alpha, err = deriveRandomness(&fs, "alpha", &proof.Z)
		if err != nil {
			chZ <- err
		}

		// Store z(g*x), without reallocating a slice
		bwsziop = bwziop.ShallowClone().Shift(1)
		bwsziop.ToLagrangeCoset(&pk.Domain[1])
		chZ <- nil
		close(chZ)
	}()

	// Full capture using latest gnark crypto...
	fic := func(fql, fqr, fqm, fqo, fqk, l, r, o fr.Element, pi2QcPrime []fr.Element) fr.Element { // TODO @Tabaie make use of the fact that qCPrime is a selector: sparse and binary

		var ic, tmp fr.Element

		ic.Mul(&fql, &l)
		tmp.Mul(&fqr, &r)
		ic.Add(&ic, &tmp)
		tmp.Mul(&fqm, &l).Mul(&tmp, &r)
		ic.Add(&ic, &tmp)
		tmp.Mul(&fqo, &o)
		ic.Add(&ic, &tmp).Add(&ic, &fqk)
		nbComms := len(commitmentInfo)
		for i := range commitmentInfo {
			tmp.Mul(&pi2QcPrime[i], &pi2QcPrime[i+nbComms])
			ic.Add(&ic, &tmp)
		}

		return ic
	}

	fo := func(l, r, o, fid, fs1, fs2, fs3, fz, fzs fr.Element) fr.Element {
		u := &pk.Domain[0].FrMultiplicativeGen
		var a, b, tmp fr.Element
		b.Mul(&beta, &fid)
		a.Add(&b, &l).Add(&a, &gamma)
		b.Mul(&b, u)
		tmp.Add(&b, &r).Add(&tmp, &gamma)
		a.Mul(&a, &tmp)
		tmp.Mul(&b, u).Add(&tmp, &o).Add(&tmp, &gamma)
		a.Mul(&a, &tmp).Mul(&a, &fz)

		b.Mul(&beta, &fs1).Add(&b, &l).Add(&b, &gamma)
		tmp.Mul(&beta, &fs2).Add(&tmp, &r).Add(&tmp, &gamma)
		b.Mul(&b, &tmp)
		tmp.Mul(&beta, &fs3).Add(&tmp, &o).Add(&tmp, &gamma)
		b.Mul(&b, &tmp).Mul(&b, &fzs)

		b.Sub(&b, &a)

		return b
	}

	fone := func(fz, flone fr.Element) fr.Element {
		one := fr.One()
		one.Sub(&fz, &one).Mul(&one, &flone)
		return one
	}

	// 0 , 1 , 2, 3 , 4 , 5 , 6 , 7, 8 , 9  , 10, 11, 12, 13, 14,  15:15+nbComm    , 15+nbComm:15+2×nbComm
	// l , r , o, id, s1, s2, s3, z, zs, ql, qr, qm, qo, qk ,lone, Bsb22Commitments, qCPrime
	fm := func(x ...fr.Element) fr.Element {

		a := fic(x[9], x[10], x[11], x[12], x[13], x[0], x[1], x[2], x[15:])
		b := fo(x[0], x[1], x[2], x[3], x[4], x[5], x[6], x[7], x[8])
		c := fone(x[7], x[14])

		c.Mul(&c, &alpha).Add(&c, &b).Mul(&c, &alpha).Add(&c, &a)

		return c
	}

	// wait for lcqk
	<-chLcqk

	// wait for Z part
	if err := <-chZ; err != nil {
		return proof, err
	}

	// wait for l, r o lagrange coset conversion
	wgLRO.Wait()

	toEval := []*iop.Polynomial{
		bwliop,
		bwriop,
		bwoiop,
		pk.lcIdIOP,
		pk.lcS1,
		pk.lcS2,
		pk.lcS3,
		bwziop,
		bwsziop,
		pk.lcQl,
		pk.lcQr,
		pk.lcQm,
		pk.lcQo,
		lcqk,
		pk.lLoneIOP,
	}
	toEval = append(toEval, lcCommitments...) // TODO: Add this at beginning
	toEval = append(toEval, pk.lcQcp...)
	systemEvaluation, err := iop.Evaluate(fm, iop.Form{Basis: iop.LagrangeCoset, Layout: iop.BitReverse}, toEval...)
	if err != nil {
		return nil, err
	}
	// open blinded Z at zeta*z
	chbwzIOP := make(chan struct{}, 1)
	go func() {
		bwziop.ToCanonical(&pk.Domain[1]).ToRegular()
		close(chbwzIOP)
	}()

	h, err := iop.DivideByXMinusOne(systemEvaluation, [2]*fft.Domain{&pk.Domain[0], &pk.Domain[1]}) // TODO Rename to DivideByXNMinusOne or DivideByVanishingPoly etc
	if err != nil {
		return nil, err
	}

	// compute kzg commitments of h1, h2 and h3
	if err := commitToQuotient(
		h.Coefficients()[:pk.Domain[0].Cardinality+2],
		h.Coefficients()[pk.Domain[0].Cardinality+2:2*(pk.Domain[0].Cardinality+2)],
		h.Coefficients()[2*(pk.Domain[0].Cardinality+2):3*(pk.Domain[0].Cardinality+2)],
		proof, pk.Kzg); err != nil {
		return nil, err
	}

	// derive zeta
	zeta, err := deriveRandomness(&fs, "zeta", &proof.H[0], &proof.H[1], &proof.H[2])
	if err != nil {
		return nil, err
	}

	// compute evaluations of (blinded version of) l, r, o, z, qCPrime at zeta
	var blzeta, brzeta, bozeta fr.Element
	qcpzeta := make([]fr.Element, len(commitmentInfo))

	var wgEvals sync.WaitGroup
	wgEvals.Add(3)
	evalAtZeta := func(poly *iop.Polynomial, res *fr.Element) {
		poly.ToCanonical(&pk.Domain[1]).ToRegular()
		*res = poly.Evaluate(zeta)
		wgEvals.Done()
	}
	go evalAtZeta(bwliop, &blzeta)
	go evalAtZeta(bwriop, &brzeta)
	go evalAtZeta(bwoiop, &bozeta)
	evalQcpAtZeta := func(begin, end int) {
		for i := begin; i < end; i++ {
			qcpzeta[i] = pk.trace.Qcp[i].Evaluate(zeta)
		}
	}
	utils.Parallelize(len(commitmentInfo), evalQcpAtZeta)

	var zetaShifted fr.Element
	zetaShifted.Mul(&zeta, &pk.Vk.Generator)
	<-chbwzIOP
	proof.ZShiftedOpening, err = kzg.Open(
		bwziop.Coefficients()[:bwziop.BlindedSize()],
		zetaShifted,
		pk.Kzg,
	)
	if err != nil {
		return nil, err
	}

	// start to compute foldedH and foldedHDigest while computeLinearizedPolynomial runs.
	computeFoldedH := make(chan struct{}, 1)
	var foldedH []fr.Element
	var foldedHDigest kzg.Digest
	go func() {
		// foldedHDigest = Comm(h1) + ζᵐ⁺²*Comm(h2) + ζ²⁽ᵐ⁺²⁾*Comm(h3)
		var bZetaPowerm, bSize big.Int
		bSize.SetUint64(pk.Domain[0].Cardinality + 2) // +2 because of the masking (h of degree 3(n+2)-1)
		var zetaPowerm fr.Element
		zetaPowerm.Exp(zeta, &bSize)
		zetaPowerm.BigInt(&bZetaPowerm)
		foldedHDigest = proof.H[2]
		foldedHDigest.ScalarMultiplication(&foldedHDigest, &bZetaPowerm)
		foldedHDigest.Add(&foldedHDigest, &proof.H[1])                   // ζᵐ⁺²*Comm(h3)
		foldedHDigest.ScalarMultiplication(&foldedHDigest, &bZetaPowerm) // ζ²⁽ᵐ⁺²⁾*Comm(h3) + ζᵐ⁺²*Comm(h2)
		foldedHDigest.Add(&foldedHDigest, &proof.H[0])                   // ζ²⁽ᵐ⁺²⁾*Comm(h3) + ζᵐ⁺²*Comm(h2) + Comm(h1)

		// foldedH = h1 + ζ*h2 + ζ²*h3
		foldedH = h.Coefficients()[2*(pk.Domain[0].Cardinality+2) : 3*(pk.Domain[0].Cardinality+2)]
		h2 := h.Coefficients()[pk.Domain[0].Cardinality+2 : 2*(pk.Domain[0].Cardinality+2)]
		h1 := h.Coefficients()[:pk.Domain[0].Cardinality+2]
		utils.Parallelize(len(foldedH), func(start, end int) {
			for i := start; i < end; i++ {
				foldedH[i].Mul(&foldedH[i], &zetaPowerm) // ζᵐ⁺²*h3
				foldedH[i].Add(&foldedH[i], &h2[i])      // ζ^{m+2)*h3+h2
				foldedH[i].Mul(&foldedH[i], &zetaPowerm) // ζ²⁽ᵐ⁺²⁾*h3+h2*ζᵐ⁺²
				foldedH[i].Add(&foldedH[i], &h1[i])      // ζ^{2(m+2)*h3+ζᵐ⁺²*h2 + h1
			}
		})
		close(computeFoldedH)
	}()

	wgEvals.Wait() // wait for the evaluations

	var (
		linearizedPolynomialCanonical []fr.Element
		linearizedPolynomialDigest    curve.G1Affine
		errLPoly                      error
	)

	// blinded z evaluated at u*zeta
	bzuzeta := proof.ZShiftedOpening.ClaimedValue

	// compute the linearization polynomial r at zeta
	// (goal: save committing separately to z, ql, qr, qm, qo, k
	// note: we linearizedPolynomialCanonical reuses bwziop memory
	linearizedPolynomialCanonical = computeLinearizedPolynomial(
		blzeta,
		brzeta,
		bozeta,
		alpha,
		beta,
		gamma,
		zeta,
		bzuzeta,
		qcpzeta,
		bwziop.Coefficients()[:bwziop.BlindedSize()],
		coefficients(cCommitments),
		pk,
	)

	// TODO this commitment is only necessary to derive the challenge, we should
	// be able to avoid doing it and get the challenge in another way
	linearizedPolynomialDigest, errLPoly = kzg.Commit(linearizedPolynomialCanonical, pk.Kzg, runtime.NumCPU()*2)
	if errLPoly != nil {
		return nil, errLPoly
	}

	// wait for foldedH and foldedHDigest
	<-computeFoldedH

	// Batch open the first list of polynomials
	polysQcp := coefficients(pk.trace.Qcp)
	polysToOpen := make([][]fr.Element, 7+len(polysQcp))
	copy(polysToOpen[7:], polysQcp)
	// offset := len(polysQcp)
	polysToOpen[0] = foldedH
	polysToOpen[1] = linearizedPolynomialCanonical
	polysToOpen[2] = bwliop.Coefficients()[:bwliop.BlindedSize()]
	polysToOpen[3] = bwriop.Coefficients()[:bwriop.BlindedSize()]
	polysToOpen[4] = bwoiop.Coefficients()[:bwoiop.BlindedSize()]
	polysToOpen[5] = pk.trace.S1.Coefficients()
	polysToOpen[6] = pk.trace.S2.Coefficients()

	digestsToOpen := make([]curve.G1Affine, len(pk.Vk.Qcp)+7)
	copy(digestsToOpen[7:], pk.Vk.Qcp)
	// offset = len(pk.Vk.Qcp)
	digestsToOpen[0] = foldedHDigest
	digestsToOpen[1] = linearizedPolynomialDigest
	digestsToOpen[2] = proof.LRO[0]
	digestsToOpen[3] = proof.LRO[1]
	digestsToOpen[4] = proof.LRO[2]
	digestsToOpen[5] = pk.Vk.S[0]
	digestsToOpen[6] = pk.Vk.S[1]

	proof.BatchedProof, err = kzg.BatchOpenSinglePoint(
		polysToOpen,
		digestsToOpen,
		zeta,
		hFunc,
		pk.Kzg,
	)

	log.Debug().Dur("took", time.Since(start)).Msg("prover done")

	if err != nil {
		return nil, err
	}

	return proof, nil

}

func coefficients(p []*iop.Polynomial) [][]fr.Element {
	res := make([][]fr.Element, len(p))
	for i, pI := range p {
		res[i] = pI.Coefficients()
	}
	return res
}

// fills proof.LRO with kzg commits of bcl, bcr and bco
func commitToLRO(bcl, bcr, bco []fr.Element, proof *Proof, kzgPk kzg.ProvingKey) error {
	n := runtime.NumCPU()
	var err0, err1, err2 error
	chCommit0 := make(chan struct{}, 1)
	chCommit1 := make(chan struct{}, 1)
	go func() {
		proof.LRO[0], err0 = kzg.Commit(bcl, kzgPk, n)
		close(chCommit0)
	}()
	go func() {
		proof.LRO[1], err1 = kzg.Commit(bcr, kzgPk, n)
		close(chCommit1)
	}()
	if proof.LRO[2], err2 = kzg.Commit(bco, kzgPk, n); err2 != nil {
		return err2
	}
	<-chCommit0
	<-chCommit1

	if err0 != nil {
		return err0
	}

	return err1
}

func commitToQuotient(h1, h2, h3 []fr.Element, proof *Proof, kzgPk kzg.ProvingKey) error {
	n := runtime.NumCPU()
	var err0, err1, err2 error
	chCommit0 := make(chan struct{}, 1)
	chCommit1 := make(chan struct{}, 1)
	go func() {
		proof.H[0], err0 = kzg.Commit(h1, kzgPk, n)
		close(chCommit0)
	}()
	go func() {
		proof.H[1], err1 = kzg.Commit(h2, kzgPk, n)
		close(chCommit1)
	}()
	if proof.H[2], err2 = kzg.Commit(h3, kzgPk, n); err2 != nil {
		return err2
	}
	<-chCommit0
	<-chCommit1

	if err0 != nil {
		return err0
	}

	return err1
}

// computeLinearizedPolynomial computes the linearized polynomial in canonical basis.
// The purpose is to commit and open all in one ql, qr, qm, qo, qk.
// * lZeta, rZeta, oZeta are the evaluation of l, r, o at zeta
// * z is the permutation polynomial, zu is Z(μX), the shifted version of Z
// * pk is the proving key: the linearized polynomial is a linear combination of ql, qr, qm, qo, qk.
//
// The Linearized polynomial is:
//
// α²*L₁(ζ)*Z(X)
// + α*( (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*Z(μζ)*s3(X) - Z(X)*(l(ζ)+β*id1(ζ)+γ)*(r(ζ)+β*id2(ζ)+γ)*(o(ζ)+β*id3(ζ)+γ))
// + l(ζ)*Ql(X) + l(ζ)r(ζ)*Qm(X) + r(ζ)*Qr(X) + o(ζ)*Qo(X) + Qk(X)
func computeLinearizedPolynomial(lZeta, rZeta, oZeta, alpha, beta, gamma, zeta, zu fr.Element, qcpZeta, blindedZCanonical []fr.Element, pi2Canonical [][]fr.Element, pk *ProvingKey) []fr.Element {

	// first part: individual constraints
	var rl fr.Element
	rl.Mul(&rZeta, &lZeta)

	// second part:
	// Z(μζ)(l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*β*s3(X)-Z(X)(l(ζ)+β*id1(ζ)+γ)*(r(ζ)+β*id2(ζ)+γ)*(o(ζ)+β*id3(ζ)+γ)
	var s1, s2 fr.Element
	chS1 := make(chan struct{}, 1)
	go func() {
		s1 = pk.trace.S1.Evaluate(zeta)                      // s1(ζ)
		s1.Mul(&s1, &beta).Add(&s1, &lZeta).Add(&s1, &gamma) // (l(ζ)+β*s1(ζ)+γ)
		close(chS1)
	}()
	// ps2 := iop.NewPolynomial(&pk.S2Canonical, iop.Form{Basis: iop.Canonical, Layout: iop.Regular})
	tmp := pk.trace.S2.Evaluate(zeta)                        // s2(ζ)
	tmp.Mul(&tmp, &beta).Add(&tmp, &rZeta).Add(&tmp, &gamma) // (r(ζ)+β*s2(ζ)+γ)
	<-chS1
	s1.Mul(&s1, &tmp).Mul(&s1, &zu).Mul(&s1, &beta) // (l(ζ)+β*s1(β)+γ)*(r(ζ)+β*s2(β)+γ)*β*Z(μζ)

	var uzeta, uuzeta fr.Element
	uzeta.Mul(&zeta, &pk.Vk.CosetShift)
	uuzeta.Mul(&uzeta, &pk.Vk.CosetShift)

	s2.Mul(&beta, &zeta).Add(&s2, &lZeta).Add(&s2, &gamma)      // (l(ζ)+β*ζ+γ)
	tmp.Mul(&beta, &uzeta).Add(&tmp, &rZeta).Add(&tmp, &gamma)  // (r(ζ)+β*u*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)
	tmp.Mul(&beta, &uuzeta).Add(&tmp, &oZeta).Add(&tmp, &gamma) // (o(ζ)+β*u²*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
	s2.Neg(&s2)                                                 // -(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)

	// third part L₁(ζ)*α²*Z
	var lagrangeZeta, one, den, frNbElmt fr.Element
	one.SetOne()
	nbElmt := int64(pk.Domain[0].Cardinality)
	lagrangeZeta.Set(&zeta).
		Exp(lagrangeZeta, big.NewInt(nbElmt)).
		Sub(&lagrangeZeta, &one)
	frNbElmt.SetUint64(uint64(nbElmt))
	den.Sub(&zeta, &one).
		Inverse(&den)
	lagrangeZeta.Mul(&lagrangeZeta, &den). // L₁ = (ζⁿ⁻¹)/(ζ-1)
						Mul(&lagrangeZeta, &alpha).
						Mul(&lagrangeZeta, &alpha).
						Mul(&lagrangeZeta, &pk.Domain[0].CardinalityInv) // (1/n)*α²*L₁(ζ)

	s3canonical := pk.trace.S3.Coefficients()
	utils.Parallelize(len(blindedZCanonical), func(start, end int) {

		var t, t0, t1 fr.Element

		for i := start; i < end; i++ {

			t.Mul(&blindedZCanonical[i], &s2) // -Z(X)*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)

			if i < len(s3canonical) {

				t0.Mul(&s3canonical[i], &s1) // (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*Z(μζ)*β*s3(X)

				t.Add(&t, &t0)
			}

			t.Mul(&t, &alpha) // α*( (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*Z(μζ)*s3(X) - Z(X)*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ))

			cql := pk.trace.Ql.Coefficients()
			cqr := pk.trace.Qr.Coefficients()
			cqm := pk.trace.Qm.Coefficients()
			cqo := pk.trace.Qo.Coefficients()
			cqk := pk.trace.Qk.Coefficients()
			if i < len(cqm) {

				t1.Mul(&cqm[i], &rl) // linPol = linPol + l(ζ)r(ζ)*Qm(X)
				t0.Mul(&cql[i], &lZeta)
				t0.Add(&t0, &t1)
				t.Add(&t, &t0) // linPol = linPol + l(ζ)*Ql(X)

				t0.Mul(&cqr[i], &rZeta)
				t.Add(&t, &t0) // linPol = linPol + r(ζ)*Qr(X)

				t0.Mul(&cqo[i], &oZeta).Add(&t0, &cqk[i])
				t.Add(&t, &t0) // linPol = linPol + o(ζ)*Qo(X) + Qk(X)

				for j := range qcpZeta {
					t0.Mul(&pi2Canonical[j][i], &qcpZeta[j])
					t.Add(&t, &t0)
				}
			}

			t0.Mul(&blindedZCanonical[i], &lagrangeZeta)
			blindedZCanonical[i].Add(&t, &t0) // finish the computation
		}
	})
	return blindedZCanonical
}
