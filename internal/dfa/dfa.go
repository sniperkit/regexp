// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dfa

import (
	"errors"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"unicode"

	"matloob.io/regexp/internal/input"
	"matloob.io/regexp/syntax"
)

const DebugDFA = false
var DebugPrintf = func(format string, a ...interface{}) {}


// -----------------------------------------------------------------------------
// search params

type searchParams struct {
	input input.Input // StringPiece
	rinput input.Rinput
	startpos int
	anchored          bool
	wantEarliestMatch bool
	runForward        bool
	start             *State
	firstbyte         int64 // int64 to be compatible with atomic ops
	failed            bool // "out" parameter: whether search gave up
	ep                int  // "out" parameter: end pointer for match

	matches []int
}

// -----------------------------------------------------------------------------
// DFA

type matchKind int

const (
	manyMatch matchKind = iota // TODO(matloob): where's this set?
	firstMatch
	longestMatch
)

type DFA struct {
	// Constant after initialization.
	prefixer input.Prefixer // TODO(matloob): this isn't set yet...
	prog            *syntax.Prog
	kind            matchKind // kind of DFA
	startUnanchored int    

	mu sync.Mutex // what does this mean: "mutex_ >= cache_mutex.r"

	//  Scratch areas, protected by mu
	q0, q1 *workq
	astack []int

	cacheMu     sync.Mutex
	memBudget   int64
	stateBudget int64 // is this used?
	bytemap     []int
	stateCache  stateSet
	start       [maxStart]startInfo // should this be a slice?
	
	
	// TODO(matloob): removeme
	reverse bool // is this a reverse DFA?
	divides []rune
}

func newDFA(prog *syntax.Prog, kind matchKind, maxMem int64) *DFA {
	d := new(DFA)
	d.prog = prog
	d.computeByteMap()
	d.kind = kind
	d.startUnanchored = prog.StartUnanchored
	d.memBudget = maxMem

	if DebugDFA {
		DebugPrintf("\nkind %d\n%v\n", kind, prog)
	}

	nmark := 0
	if kind == longestMatch {
		nmark = len(prog.Inst)
	}
	nastack := 2*len(prog.Inst) + nmark

	for i := range d.start {
		d.start[i].firstbyte = fbUnknown
	}

	// Account for space needed for DFA, q0, q1, astack.
	/* TODO(matloob): DO state memory budget stuff */
	d.stateBudget = d.memBudget
	d.stateCache.init(int(maxMem))

	d.q0 = newWorkq(len(prog.Inst), nmark)
	d.q1 = newWorkq(len(prog.Inst), nmark)
	d.astack = make([]int, nastack)

	return d
}

func newReverseDFA(prog *syntax.Prog, kind matchKind, maxMem int64) *DFA {
	d := newDFA(prog, kind, maxMem)
	d.reverse = true
	return d
}

var errFallBack = errors.New("falling back to NFA")

func (d *DFA) search(i input.Input, startpos int, reversed *DFA) (start int, end int, matched bool, err error) {
	defer func() {
/*		if r := recover(); r != nil {
			if rerr, ok := r.(error); ok {
				err = rerr
			}
			err = errors.New("panicked in RE execution")
		}*/
	}()
	params := searchParams{}
	params.startpos = startpos
	params.wantEarliestMatch = false
	params.input = i
	params.runForward = true
	params.ep = int(math.MaxInt64)
	if !d.analyzeSearch(&params) {
		return -1, -1, false, errors.New("analyze search failed on forward DFA")
	}
	b := d.searchLoop(&params)
	if params.failed {
		return -1, -1, false, errFallBack
	}
	if !b  {
		return -1, -1, false, nil
	}
	end = params.ep

	params = searchParams{}
	params.startpos = startpos
	params.ep = end
	params.anchored = true
	// params.wantEarliestMatch = true
	params.input = i
	params.rinput= reverse(i)
	params.runForward = false
	if !reversed.analyzeSearch(&params) {
		return -2, -2, false, errors.New("analyze search failed on reverse DFA")
	}
	b = reversed.searchLoop(&params)
	if DebugDFA {
		DebugPrintf("\nkind %d\n%v\n", d.kind, d.prog)
	}
	if params.failed {
		return -1, -1, false, errFallBack
	}
	return params.ep, end, b, nil
}

func reverse(i input.Input) input.Rinput {
	if ri, ok := i.(input.Rinput); ok {
		return ri
	}
	return nil
}

func (d *DFA) loadNextState(from *State, r rune) *State {
	// TODO(matloob): Atomize this once *States are indexes into state array...
	return from.next[d.byteMap(r)]
}

func (d *DFA) storeNextState(from *State, r rune, to *State) {
	// TODO(matloob): Atomize this once *States are indexes into state array...
	from.next[d.byteMap(r)] = to
}

func (d *DFA) analyzeSearch(params *searchParams) bool {
	input := params.input

	// Sanity check: make sure that text lies within context
	// TODO(matloob): is this necessary?

	// Determine correct search type.
	var start int
	var flags flag
	if params.runForward {
		flags =  flag(input.Context(params.startpos))
		if flags & flag(syntax.EmptyBeginText) == 0 {
			if  r, _ := reverse(input).Rstep(params.startpos); syntax.IsWordChar(r) {
				flags |= flagLastWord
			}
		}
	} else {
		flags = flag(params.rinput.Context(params.ep))
		// reverse the flag -- do this a nicer way!
		flags = flag(int(flags) & ^0xF) |((flags & 0xA) >> 1) | ((flags & 0x5) << 1)
		if flags & flag(syntax.EmptyBeginText) == 0 {
			if  r, _ := params.input.Step(params.ep); syntax.IsWordChar(r) {
				flags |= flagLastWord
			}
		}
	}

	if flags & flag(syntax.EmptyBeginText) != 0{
		start |= startBeginText
	} else if flags & flag(syntax.EmptyBeginLine) != 0 {
		start |= startBeginLine
	} else if flags & flag(syntax.EmptyWordBoundary) != 0 {
		start |= startWordBoundary
	} else {
		start |= startNonWordBoundary
	}
	if params.anchored /* || prog.anchorStart() */ {
		start |= kStartAnchored
	}
	info := d.start[start]

	if !d.analyzeSearchHelper(params, &info, flags) {
		panic("failed to analyze start state") // LOG(DFATAL)
		params.failed = true
		return false
	}

	params.start = info.start
	params.firstbyte = atomic.LoadInt64(&info.firstbyte)

	return true
}

func (d *DFA) analyzeSearchHelper(params *searchParams, info *startInfo, flags flag) bool {
	// Quick check;
	fb := atomic.LoadInt64(&info.firstbyte)
	if fb != fbUnknown {
		return true
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if info.firstbyte != fbUnknown {
		return true
	}

	d.q0.clear()
	s := d.prog.Start
	if !params.anchored {
		s = d.startUnanchored
	}
	d.addToQueue(d.q0, s, flags)
	info.start = d.workqToCachedState(d.q0, flags)
	if info.start == nil {
		return false
	}

	if info.start == deadState {
		// Synchronize with "quick check" above.
		atomic.StoreInt64(&info.firstbyte, fbNone)
		return true
	}

	if info.start == fullMatchState {
		// Synchronize with "quick check" above.
		atomic.StoreInt64(&info.firstbyte, fbNone)
		return true
	}

	// Synchronize with "quick check" above.
	atomic.StoreInt64(&info.firstbyte, fbNone)
	return true

}

// Processes input rune r in state, returning new state.
// Caller does not hold mutex.
func (d *DFA) runStateOnRuneUnlocked(state *State,  r rune) *State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runStateOnRune(state, r)
}

// Looks up bytes in d.bytemap but handles case c == kByteEndText too.
func (d *DFA) byteMap(r rune) int {
	// Use the trivial byte map for now...
	// See ComputeByteMap
	if r == input.EndOfText {
		return len(d.divides);
	}
	if r == input.StartOfText {
		return len(d.divides) + 1
	}
	if r > 255 {
		lo, hi := 0, len(d.divides)
		for {
			// search d.divides
			center := (lo+hi)/2
			if center == lo {
				return lo
			}
			divcenter := d.divides[center]
			if r >= divcenter {
				lo = center
			} else {
				hi = center
			}
		}
	}
	return d.bytemap[int(r)]
}

func (d *DFA) computeByteMap() {
	divides := make(map[rune]bool)
	for _, inst := range d.prog.Inst {
		switch inst.Op {
		case syntax.InstRune:
			if len(inst.Rune) == 1 {
				r0 :=inst.Rune[0]
				divides[r0] = true
				divides[r0+1] = true
				if syntax.Flags(inst.Arg)&syntax.FoldCase != 0 {
					for r1 := unicode.SimpleFold(r0); r1 != r0; r1 = unicode.SimpleFold(r1) {
									
						divides[r1] = true
						
						divides[r1+1] = true
					}
				}
				break
			} else {		
				for i := 0; i < len(inst.Rune); i += 2 {
					divides[inst.Rune[i]] = true
					divides[inst.Rune[i+1] +1] = true
					if syntax.Flags(inst.Arg)&syntax.FoldCase != 0 {
						rl := inst.Rune[i]
						rh:=inst.Rune[i+1]
						for r0 := rl; r0 <= rh; r0++ {
							// range mapping doesn't commute...
							for r1 := unicode.SimpleFold(r0); r1 != r0; r1 = unicode.SimpleFold(r1) {
								divides[r1] = true
						
								divides[r1+1] = true
							}
						}
					}
				}
			}

		case syntax.InstRune1:
			r0 := inst.Rune[0]
			divides[r0] = true
			divides[r0 + 1]  = true
			if syntax.Flags(inst.Arg)&syntax.FoldCase != 0 {
				for r1 := unicode.SimpleFold(r0); r1 != r0; r1 = unicode.SimpleFold(r1) {
					divides[r1] = true
					divides[r1+1] = true
				}
			}
		case syntax.InstRuneAnyNotNL: 
			divides['\n'] = true
			divides['\n'+1] = true
		
		
		case syntax.InstEmptyWidth: 
			switch syntax.EmptyOp(inst.Arg) {
				case syntax.EmptyBeginLine, syntax.EmptyEndLine:
					divides['\n'] = true
					divides['\n'+1] = true
				case syntax.EmptyWordBoundary, syntax.EmptyNoWordBoundary:
					// can we turn this into an InstRune?
					divides['A'] = true
					divides['Z'+1] = true
					divides['a'] = true
					divides['z' + 1] = true
					divides['0'] = true
					divides['9'+1] = true
					divides['_'] = true
					divides['_'+1] = true
			}
		}
	}

	
	divl := make([]rune, 0,len(divides))
	divl = append(divl, -1)
	for r := range divides {
		divl = append(divl, r)
	}
	runeSlice(divl).Sort()
	d.divides = divl
	d.bytemap = make([]int, 256)
	k := 0
	for i := range d.bytemap {
		if divides[rune(i)] {
			k++
		}
		d.bytemap[i] = k
	} 
}

// runeSlice exists to permit sorting the case-folded rune sets.
type runeSlice []rune

func (p runeSlice) Len() int           { return len(p) }
func (p runeSlice) Less(i, j int) bool { return p[i] < p[j] }
func (p runeSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// Sort is a convenience method.
func (p runeSlice) Sort() {
	sort.Sort(p)
}


// Processes input rune r in state, returning new state.
func (d *DFA) runStateOnRune(state *State, r rune) *State {
	if isSpecialState(state) {
		if state == fullMatchState {
			// It is convenient for routines like PossibleMatchRange
			// if we implement RunStateOnByte for FullMatchState:
			// once you get into this state you never get out,
			// so it's pretty easy.
			return fullMatchState
		}
		if state == deadState {
			panic("dead state in runStateOnRune") // DFATAL
		}
		if state == nil {
			panic("nil state in runStateOnRune") // DFATAL
		}
		panic("unexpected special state in runStateOnRune") // DFATAL
	}

	// If someone else already computed this, return it.
	var ns *State
	if !(d.byteMap(r) < len(state.next)) {
		// TODO(matloob): return this as an error?
		panic(errors.New("byte range index is greater than number out arrows from state"))
	}
	ns = d.loadNextState(state, r)
	if ns != nil {
		return ns
	}
	// Convert state to workq.
	d.stateToWorkq(state, d.q0)
 
	// Flags marking the kinds of empty-width things (^ $ etc)
	// around this byte.  Before the byte we have the flags recorded
	// in the State structure itself.  After the byte we have
	// nothing yet (but that will change: read on).
	needflag := state.flag >> flagNeedShift 
	beforeflag := state.flag & flagEmptyMask
	oldbeforeflag := beforeflag
	afterflag := flag(0)

	if r == '\n' {
		// Insert implicit $ and ^ around \n
		beforeflag |= flag(syntax.EmptyEndLine)
		afterflag |= flag(syntax.EmptyBeginLine)
	}

	if r == input.EndOfText {
	// Insert implicit $ and \z before the fake "end text" byte.
		beforeflag |= flag(syntax.EmptyEndLine) | flag(syntax.EmptyEndText)
	} else if r == input.StartOfText {
		beforeflag |= flag(syntax.EmptyBeginLine) | flag(syntax.EmptyBeginText)	
	}	

	// The state flag flagLastWord says whether the last
	// byte processed was a word character.  Use that info to
	// insert empty-width (non-)word boundaries.
	islastword := state.flag&flagLastWord != 0
	isword := r != input.EndOfText && syntax.IsWordChar(r)
	if isword == islastword {
		beforeflag |= flag(syntax.EmptyNoWordBoundary)
	} else {
		beforeflag |= flag(syntax.EmptyWordBoundary)
	}
	

	// Okay, finally ready to run.
	// Only useful to rerun on empty string if there are new, useful flags.
	if beforeflag & ^oldbeforeflag & needflag != 0 {
		d.runWorkqOnEmptyString(d.q0, d.q1, beforeflag)
		d.q0, d.q1 = d.q1, d.q0
	}
	ismatch := false
	d.runWorkqOnRune(d.q0, d.q1, r, afterflag, &ismatch, d.kind)

	// Most of the time, we build the state from the output of
	// RunWorkqOnByte, so swap q0_ and q1_ here.  However, so that
	// RE2::Set can tell exactly which match instructions
	// contributed to the match, don't swap if c is kByteEndText.
	// The resulting state wouldn't be correct for further processing
	// of the string, but we're at the end of the text so that's okay.
	// Leaving q0_ alone preseves the match instructions that led to
	// the current setting of ismatch.
	if r != input.EndOfText || d.kind != manyMatch {
		d.q0, d.q1 = d.q1, d.q0
	}

	flag := afterflag
	if ismatch {
		flag |= flagMatch
	}
	if isword {
		flag |= flagLastWord
	}

	ns = d.workqToCachedState(d.q0, flag)

	// Flush ns before linking to it.
	// Write barrier before updating state->next_ so that the
	// main search loop can proceed without any locking, for speed.
	// (Otherwise it would need one mutex operation per input byte.)
	d.storeNextState(state, r, ns)

	return ns
}

// Looks in the State cache for a State matching q, flag.
// If one is found, returns it.  If one is not found, allocates one,
// inserts it in the cache, and returns it.
func (d *DFA) workqToCachedState(q *workq, flags flag) *State {
	// if DEBUG_MODE { d.mu.AssertHeld() }

	// Construct array of instruction ids for the new state.
	// Only ByteRange, EmptyWidth, and Match instructions are useful to keep:
	// those are the only operators with any effect in
	// RunWorkqOnEmptyString or RunWorkqOnByte.

	// TODO(matloob): This escapes... is that ok or do we need to be more careful here?
	ids := make([]int, q.size()) // should we have a sync.pool of these?
	n := 0
	needflags := flag(0) // flags needed by InstEmptyWidth instructions
	sawmatch := false    // whether queue contains guaranteed InstMatch
	sawmark := false     // whether queue contains a mark
	if DebugDFA {
		DebugPrintf("WorkqToCachedState %s [%x]", q.dump(), flags)
	}
	for i, id := range q.elements() {
		if sawmatch && (d.kind == firstMatch || q.isMark(id)) {
			break
		}
		if q.isMark(id) {
			if n > 0 && ids[n-1] != int(mark) {
				sawmark = true
				ids[n] = int(mark) 
				n++
			}
			continue
		}
		inst := d.prog.Inst[id]
		switch inst.Op {
		case syntax.InstAltMatch:
			// This state will continue to a match no matter what
			// the rest of the input is.  If it is the highest priority match
			// being considered, return the special FullMatchState
			// to indicate that it's all matches from here out.
			if d.kind != manyMatch &&
				(d.kind != firstMatch ||
					(i == 0 && greedy(inst, d.prog))) && // greedy?? dfa.cc:639
				(d.kind != longestMatch || !sawmark) &&
				(flags&flagMatch != 0) { // TODO(matloob): another conversion
				// delete[] ids
				if DebugDFA {
					DebugPrintf(" -> FullMatchState\n")
				}
				return fullMatchState
			}
			fallthrough
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL,
			syntax.InstEmptyWidth, syntax.InstMatch, // These are useful.
			syntax.InstAlt: //  Not useful, but necessary [*]
			ids[n] = id
			n++
			if inst.Op == syntax.InstEmptyWidth {
				needflags |= flag(inst.Arg)
			}
			if inst.Op == syntax.InstMatch {
				sawmatch = true
			}

		default: // The rest are not.
			break
		}
		// [*] kInstAlt would seem useless to record in a state, since
		// we've already followed both its arrows and saved all the
		// interesting states we can reach from there.  The problem
		// is that one of the empty-width instructions might lead
		// back to the same kInstAlt (if an empty-width operator is starred),
		// producing a different evaluation order depending on whether
		// we keep the kInstAlt to begin with.  Sigh.
		// A specific case that this affects is /(^|a)+/ matching "a".
		// If we don't save the kInstAlt, we will match the whole "a" (0,1)
		// but in fact the correct leftmost-first match is the leading "" (0,0).

	}
	// DCHECK_LE(n, q.size())
	if n > 0 && ids[n-1] == int(mark) {
		n--
	}

	// If there are no empty-width instructions waiting to execute,
	// then the extra flag bits will not be used, so there is no
	// point in saving them.  (Discarding them reduces the number
	// of distinct states.)
	if needflags == 0 {
		flags &= flagMatch
	}

	// NOTE(rsc): The code above cannot do flag &= needflags,
	// because if the right flags were present to pass the current
	// kInstEmptyWidth instructions, new kInstEmptyWidth instructions
	// might be reached that in turn need different flags.
	// The only sure thing is that if there are no kInstEmptyWidth
	// instructions at all, no flags will be needed.
	// We could do the extra work to figure out the full set of
	// possibly needed flags by exploring past the kInstEmptyWidth
	// instructions, but the check above -- are any flags needed
	// at all? -- handles the most common case.  More fine-grained
	// analysis can only be justified by measurements showing that
	// too many redundant states are being allocated.

	// If there are no Insts in the list, it's a dead state,
	// which is useful to signal with a special pointer so that
	// the execution loop can stop early.  This is only okay
	// if the state is *not* a matching state.
	if n == 0 && flags == 0 {
		// delete[] inst
		if DebugDFA {
			DebugPrintf(" -> DeadState\n")
		}
		return deadState
	}

	// Reslice ids to contain only the parts we need.
	ids = ids[:n]

	// If we're in longest match mode, the state is a sequence of
	// unordered state sets separated by Marks.  Sort each set
	// to canonicalize, to reduce the number of distinct sets stored.
	if d.kind == longestMatch {
		i := 0 // ids[i:markp] will contain each set
		for i < len(ids) {
			markp := i // markIdx?
			for markp < len(ids) && ids[markp] != int(mark) {
				markp++
			}
			sort.Ints(ids[i:markp])
			if markp < len(ids) {
				markp++
			}
			i = markp
		}
	}

	// Save the needed empty-width flags in the top bits for use later.
	flags |= needflags << flagNeedShift
	state := d.cachedState(ids, flags)
	/* delete[] ids */
	return state
}

// see dfa.cc:639
func greedy(syntax.Inst, *syntax.Prog) bool {
	panic("not implemented")
}

// ids is a list of indexes into prog.Inst
func (d *DFA) cachedState(ids []int, flags flag) *State {
	// if DEBUG_MODE { d.mu.assertHeld() }

	// Look in the cache for a pre-existing state.
	f := d.stateCache.find(ids, flags)
	if f != nil {
		if DebugDFA {
			DebugPrintf(" -cached-> %s\n", f.Dump())
		}
		return f
	}

	// TODO(matloob): state cache hash table comment not accurate!!!
	// Must have enough memory for new state.
	// In addition to what we're going to allocate,
	// the state cache hash table seems to incur about 32 bytes per
	// *State, empirically.
	// TODO(matloob): COMPLETELY IGNORING MEM BUDGET WARNING!!!

	// Allocate new state, along with room for next and inst.
	// TODO(matloob): this code does a bunch of UNSAFE stuff...


	nextsize := len(d.divides)+2
	state := d.stateCache.insert(ids, flags, nextsize)
	if DebugDFA {
		DebugPrintf(" -> %s\n",  state.Dump())
	}
	
	return state
}

// Adds ip to the work queue, following empty arrows according to flag
// and expanding kInstAlt instructions (two-target gotos).
func (d *DFA) addToQueue(q *workq, id int, flags flag) {
	stk := d.astack
	nstk := 0 // TODO(matloob): reslice the stack and use len(stk) instead of nstk??

	stk[nstk] = id
	nstk++

	for nstk > 0 {
		// DCHECK_LE(nstk, nastack)
		nstk--
		id = stk[nstk]

		if id == int(mark) {
			q.mark()
			continue
		}

		if id == 0 {
			continue
		}

		// If ip is already on the queue, nothing to do.
		// Otherwise add it.  We don't actually keep all the ones
		// that get added -- for example, kInstAlt is ignored
		// when on a work queue -- but adding all ip's here
		// increases the likelihood of q->contains(id),
		// reducing the amount of duplicated work.
		if q.contains(id) {
			continue
		}
		q.insertNew(id)

		// Process instruction.
		inst := d.prog.Inst[id]
		switch inst.Op {
		case syntax.InstFail: // can't happen: discarded above
			break
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL, syntax.InstMatch: // just save these on the queue
			break
		case syntax.InstCapture, syntax.InstNop:
			stk[nstk] = int(inst.Out)
			nstk++

		case syntax.InstAlt, syntax.InstAltMatch: // two choices: expand both, in order
			stk[nstk] = int(inst.Arg)
			nstk++
			if q.maxmark() > 0 && id == d.prog.StartUnanchored && id != d.prog.Start {
				stk[nstk] = int(mark)
				nstk++
			}
			stk[nstk] = int(inst.Out)
			nstk++
			break

		case syntax.InstEmptyWidth:
			empty := flag(inst.Arg)
			if empty&flags == empty {
				stk[nstk] = int(inst.Out)
				nstk++
			}
			break
		}
	}

}

func (d *DFA) stateToWorkq(s *State, q *workq) {
	q.clear()
	for i := range s.inst {
		if s.inst[i] == int(mark) {
			q.mark()
		} else {
			q.insertNew(int(s.inst[i]))
		}
	}
}

func (d *DFA) runWorkqOnEmptyString(oldq *workq, newq *workq, flag flag) {
	newq.clear()
	for _, si /* name it inst?? */ := range oldq.elements() {
		if oldq.isMark(si) {
			d.addToQueue(newq, int(mark), flag)
		} else {
			d.addToQueue(newq, si, flag)
		}
	}
}

// Runs a Workq on a given rune followed by a set of empty-string flags,
// producing a new Workq in nq.  If a match instruction is encountered,
// sets *ismatch to true.
// L >= mutex_
//
// Runs the work queue, processing the single byte c followed by any empty
// strings indicated by flag.  For example, c == 'a' and flag == kEmptyEndLine,
// means to match c$.  Sets the bool *ismatch to true if the end of the
// regular expression program has been reached (the regexp has matched).
func (d *DFA) runWorkqOnRune(oldq *workq, newq *workq, r rune, flag flag, ismatch *bool, kind matchKind) {
	// if DEBUG_MODE { d.mu.assertHeld() }

	newq.clear()
	for _, id := range oldq.elements() {
		if oldq.isMark(id) {
			if *ismatch {
				return
			}
			newq.mark()
			continue
		}
		inst := d.prog.Inst[id]
		switch inst.Op {
		case syntax.InstFail: // never succeeds
		case syntax.InstCapture: // already followed
		case syntax.InstNop: // already followed
		case syntax.InstAlt: // already followed
		case syntax.InstAltMatch: // already followed
		case syntax.InstEmptyWidth: // already followed
			break

			// TODO(matloob): do we want inst.op() to merge the cases?
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			if inst.MatchRune(r) {
				d.addToQueue(newq, int(inst.Out), flag)
			}
			break

		case syntax.InstMatch:
			*ismatch = true
			if kind == firstMatch {
				return
			}
			break
		}
	}

	if DebugDFA {
		DebugPrintf("%s on %d[%x] -> %s [%v]\n",
			oldq.dump(), r, flag, newq.dump(), *ismatch)
	}

}


//////////////////////////////////////////////////////////////////////
//
// DFA execution.
//
// The basic search loop is easy: start in a state s and then for each
// byte c in the input, s = s->next[c].
//
// This simple description omits a few efficiency-driven complications.
//
// First, the State graph is constructed incrementally: it is possible
// that s->next[c] is null, indicating that that state has not been
// fully explored.  In this case, RunStateOnByte must be invoked to
// determine the next state, which is cached in s->next[c] to save
// future effort.  An alternative reason for s->next[c] to be null is
// that the DFA has reached a so-called "dead state", in which any match
// is no longer possible.  In this case RunStateOnByte will return NULL
// and the processing of the string can stop early.
//
// Second, a 256-element pointer array for s->next_ makes each State
// quite large (2kB on 64-bit machines).  Instead, dfa->bytemap_[]
// maps from bytes to "byte classes" and then next_ only needs to have
// as many pointers as there are byte classes.  A byte class is simply a
// range of bytes that the regexp never distinguishes between.
// A regexp looking for a[abc] would have four byte ranges -- 0 to 'a'-1,
// 'a', 'b' to 'c', and 'c' to 0xFF.  The bytemap slows us a little bit
// but in exchange we typically cut the size of a State (and thus our
// memory footprint) by about 5-10x.  The comments still refer to
// s->next[c] for simplicity, but code should refer to s->next_[bytemap_[c]].
//
// Third, it is common for a DFA for an unanchored match to begin in a
// state in which only one particular byte value can take the DFA to a
// different state.  That is, s->next[c] != s for only one c.  In this
// situation, the DFA can do better than executing the simple loop.
// Instead, it can call memchr to search very quickly for the byte c.
// Whether the start state has this property is determined during a
// pre-compilation pass, and if so, the byte b is passed to the search
// loop as the "firstbyte" argument, along with a boolean "have_firstbyte".
//
// Fourth, the desired behavior is to search for the leftmost-best match
// (approximately, the same one that Perl would find), which is not
// necessarily the match ending earliest in the string.  Each time a
// match is found, it must be noted, but the DFA must continue on in
// hope of finding a higher-priority match.  In some cases, the caller only
// cares whether there is any match at all, not which one is found.
// The "want_earliest_match" flag causes the search to stop at the first
// match found.
//
// Fifth, one algorithm that uses the DFA needs it to run over the
// input string backward, beginning at the end and ending at the beginning.
// Passing false for the "run_forward" flag causes the DFA to run backward.
//
// The checks for these last three cases, which in a naive implementation
// would be performed once per input byte, slow the general loop enough
// to merit specialized versions of the search loop for each of the
// eight possible settings of the three booleans.  Rather than write
// eight different functions, we write one general implementation and then
// inline it to create the specialized ones.
//
// Note that matches are delayed by one byte, to make it easier to
// accomodate match conditions depending on the next input byte (like $ and \b).
// When s->next[c]->IsMatch(), it means that there is a match ending just
// *before* byte c.

// The generic search loop.  Searches text for a match, returning
// the pointer to the end of the chosen match, or NULL if no match.
// The bools are equal to the same-named variables in params, but
// making them function arguments lets the inliner specialize
// this function to each combination (see two paragraphs above).
func (d *DFA) searchLoop(params *searchParams) bool {
	haveFirstbyte := params.firstbyte >= 0
	wantEarliestMatch := params.wantEarliestMatch
	runForward := params.runForward

	start := params.start
	bp := 0 // start of text
	p := params.startpos  // text scanning point
	ep := params.ep
	resetp := -1
	if !runForward {
		p, ep = ep, p 
	} 

	var saveS, saveStart stateSaver

	// const uint8* bytemap = prog_->bytemap()
	var lastMatch int = -1 // most recent matching position in text
	matched := false
	s := start

	if s.isMatch() { 
		matched = true
		lastMatch = p
		if wantEarliestMatch {
			params.ep = lastMatch
			return true
		}
	}

	var w int
	for p != ep {
		if DebugDFA {
			DebugPrintf("@%d: %s\n", p - bp, s.Dump())
		}
		if haveFirstbyte && s == start {
			// TODO(matloob): Correct the comment
			// In start state, only way out is to find firstbyte,
			// so use optimized assembly in memchr to skip ahead.
			// If firstbyte isn't found, we can skip to the end
			// of the string.
			if runForward {
				p = params.input.Index(d.prefixer, p)
				if p < 0 {
					p = ep
					break
				}
			} else {
				// p = params.input.rindex(d.prefixer, ep)
				if p < 0 {
					p = ep
					break
				}
				p++
			}
		}

		var r rune
		if runForward {
			r, w = params.input.Step(p)
			p += w
		} else {
			r, w = params.rinput.Rstep(p)
			p -= w
		}
		if r == input.EndOfText  {
			break
		}

		// Note that multiple threads might be consulting
		// s->next_[bytemap[c]] simultaneously.
		// RunStateOnByte takes care of the appropriate locking,
		// including a memory barrier so that the unlocked access
		// (sometimes known as "double-checked locking") is safe.
		// The  alternative would be either one DFA per thread
		// or one mutex operation per input byte.
		//
		// ns == DeadState means the state is known to be dead
		// (no more matches are possible).
		// ns == NULL means the state has not yet been computed
		// (need to call RunStateOnByteUnlocked).
		// RunStateOnByte returns ns == NULL if it is out of memory.
		// ns == FullMatchState means the rest of the string matches.
		ns := d.loadNextState(s, r)
		if ns == nil {
			ns = d.runStateOnRuneUnlocked(s, r)
			if ns == nil {
				panic("state saving stuff not implemented")
				// After we reset the cache, we hold cache_mutex exclusively,
				// so if resetp != NULL, it means we filled the DFA state
				// cache with this search alone (without any other threads).
				// Benchmarks show that doing a state computation on every
				// byte runs at about 0.2 MB/s, while the NFA (Match) can do the
				// same at about 2 MB/s.  Unless we're processing an average
				// of 10 bytes per state computation, fail so that RE2 can
				// fall back to the NFA.
				if p >= 0 && p - resetp < 10*d.stateCache.size() {
					params.failed = true
					return false
				}
				resetp = p
				
				// Prepare to save start and s across the reset.
				saveStart.Save(d, start)
				saveS.Save(d, s)

				// Discard all the States in the cache.
				d.resetCache()
				 
				// Restore start and s so we can continue.
				if start, s := saveStart.Restore(), saveS.Restore(); start == nil || s == nil {
					params.failed = true
					return false
				}
				ns = d.runStateOnRuneUnlocked(s, r)
				if ns == nil {
					params.failed = true
					return false
				}	  
			}
	
		}
	
		//  if (ns <= SpecialStateMax) {
		if isSpecialState(ns) {
			if ns == deadState {
				params.ep = lastMatch
				return matched
			}
			params.ep = ep
			return true
		}
		s = ns
	
		if s.isMatch(){
			matched = true
			// The DFA notices the match one rune late,
			// so adjust p before using it in the match.
			if runForward {
				lastMatch = p - w
			} else {
				lastMatch = p + w
			}
			if DebugDFA {
				DebugPrintf("match @%d! [%s]\n", lastMatch - bp, s.Dump())
			}
			if wantEarliestMatch {
				params.ep = lastMatch
				return true
			}
		}
	}

	// Process one more byte to see if it triggers a match.
	// (Remember, matches are delayed one byte.)
	lastbyte := input.EndOfText // TODO(matloob): not really a byte...

	ns := d.loadNextState(s, lastbyte)
	if ns == nil {
		ns = d.runStateOnRuneUnlocked(s, lastbyte)
		if ns == nil {
			saveS.Save(d, s)
			d.resetCache()
			if s = saveS.Restore(); s == nil {
				params.failed = true
				return false
			}
			ns = d.runStateOnRuneUnlocked(s, lastbyte)
			if ns != nil {
				params.failed = true
				return false
			}
		}
	}

	s = ns
	if DebugDFA {
		DebugPrintf("@_: %s\n", s.Dump())
	}
	if s == fullMatchState {
		params.ep = ep
		return true
	}
	if !isSpecialState(s) && s.isMatch() {
		matched = true
		lastMatch = p
		// TODO(matloob): Just remove this? Do we support ManyMatch?
		if params.matches != nil && false /* && d.kind == ManyMatch */ {
			v := params.matches
			v = v[:0] // TODO(matloob): just operate on params.matches?
			for i := range s.inst {
				inst := d.prog.Inst[s.inst[i]]
				if inst.Op == syntax.InstMatch {
					v = append(v, 0 /* inst.matchID() */ ) // TODO(matloob): match id?
				}
			}
			params.matches = v
		}
	}
	params.ep = lastMatch
	return matched
}

func (d *DFA) resetCache() {
	d.cacheMu.Lock()
	
	for i := range d.start {
		d.start[i].start = nil
		atomic.StoreInt64(&d.start[i].firstbyte, fbUnknown)
	}
	d.stateCache.clear()
	
	d.cacheMu.Unlock()
}