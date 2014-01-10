package pdp11

import "fmt"

const (
	FLAGN = 8
	FLAGZ = 4
	FLAGV = 2
	FLAGC = 1
)

const pr = false // debug

var (
	curuser, prevuser bool
	clkcounter        int
	waiting           = false
	interrupts        []intr
)

type intr struct{ vec, pri int }

// traps
const (
	INTBUS    = 0004
	INTINVAL  = 0010
	INTDEBUG  = 0014
	INTIOT    = 0020
	INTTTYIN  = 0060
	INTTTYOUT = 0064
	INTFAULT  = 0250
	INTCLOCK  = 0100
	INTRK     = 0220
)

var bootrom = [...]uint16{
	0042113,        /* "KD" */
	0012706, 02000, /* MOV #boot_start, SP */
	0012700, 0000000, /* MOV #unit, R0        ; unit number */
	0010003,          /* MOV R0, R3 */
	0000303,          /* SWAB R3 */
	0006303,          /* ASL R3 */
	0006303,          /* ASL R3 */
	0006303,          /* ASL R3 */
	0006303,          /* ASL R3 */
	0006303,          /* ASL R3 */
	0012701, 0177412, /* MOV #RKDA, R1        ; csr */
	0010311,          /* MOV R3, (R1)         ; load da */
	0005041,          /* CLR -(R1)            ; clear ba */
	0012741, 0177000, /* MOV #-256.*2, -(R1)  ; load wc */
	0012741, 0000005, /* MOV #READ+GO, -(R1)  ; read & go */
	0005002,        /* CLR R2 */
	0005003,        /* CLR R3 */
	0012704, 02020, /* MOV #START+20, R4 */
	0005005, /* CLR R5 */
	0105711, /* TSTB (R1) */
	0100376, /* BPL .-2 */
	0105011, /* CLRB (R1) */
	0005007, /* CLR PC */
}

func xor(x, y int) int {
	a := x & y
	b := ^x & ^y
	z := ^a & ^b
	return z
}

func xor16(x, y uint16) uint16 {
	a := x & y
	b := ^x & ^y
	z := ^a & ^b
	return z
}

type KB11 struct {
	R        [8]uint16 // registers
	PS       uint16    // processor status
	PC       uint16    // address of current instruction
	KSP, USP uint16    // kernel and user stack pointer
	SR0, SR2 uint16

	Input  chan uint8
	unibus *Unibus
}

func (k *KB11) switchmode(newm bool) {
	prevuser = curuser
	curuser = newm
	if prevuser {
		k.USP = k.R[6]
	} else {
		k.KSP = k.R[6]
	}
	if curuser {
		k.R[6] = k.USP
	} else {
		k.R[6] = k.KSP
	}
	k.PS &= 0007777
	if curuser {
		k.PS |= (1 << 15) | (1 << 14)
	}
	if prevuser {
		k.PS |= (1 << 13) | (1 << 12)
	}
}

func (k *KB11) decode(a uint16, w, user bool) int {
	var p page
	if !(k.SR0&1 == 1) {
		if a := int(a); a >= 0170000 {
			a += 0600000
			return a
		} else {
			return a
		}
	}
	if user {
		p = pages[(a>>13)+8]
	} else {
		p = pages[(a >> 13)]
	}
	if w && !p.write {
		k.SR0 = (1 << 13) | 1
		k.SR0 |= a >> 12 & ^uint16(1)
		if user {
			k.SR0 |= (1 << 5) | (1 << 6)
		}
		k.SR2 = k.PC
		panic(trap{INTFAULT, "write to read-only page " + ostr(a, 6)})
	}
	if !p.read {
		k.SR0 = (1 << 15) | 1
		k.SR0 |= (a >> 12) & ^uint16(1)
		if user {
			k.SR0 |= (1 << 5) | (1 << 6)
		}
		k.SR2 = k.PC
		panic(trap{INTFAULT, "read from no-access page " + ostr(a, 6)})
	}
	block := a >> 6 & 0177
	disp := int(a & 077)
	if p.ed && block < p.len || !p.ed && block > p.len {
		//if(p.ed ? (block < p.len) : (block > p.len)) {
		k.SR0 = (1 << 14) | 1
		k.SR0 |= (a >> 12) & ^uint16(1)
		if user {
			k.SR0 |= (1 << 5) | (1 << 6)
		}
		k.SR2 = k.PC
		panic(trap{INTFAULT, "page length exceeded, address " + ostr(a, 6) + " (block " + ostr(block, 3) + ") is beyond length " + ostr(p.len, 3)})
	}
	if w {
		p.pdr |= 1 << 6
	}
	return (int(block+p.addr) << 6) + disp
}

func (k *KB11) read8(a uint16) uint16 {
	return k.unibus.physread8(k.decode(a, false, curuser))
}

func (k *KB11) read16(a uint16) uint16 {
	return k.unibus.physread16(k.decode(uint16(a), false, curuser))
}

func (k *KB11) write8(a, v uint16) {
	k.unibus.physwrite8(k.decode(a, true, curuser), v)
}

func (k *KB11) write16(a, v uint16) {
	k.unibus.physwrite16(k.decode(a, true, curuser), v)
}

func (k *KB11) fetch16() uint16 {
	val := k.read16(k.R[7])
	k.R[7] += 2
	return val
}

func (k *KB11) push(v uint16) {
	k.R[6] -= 2
	k.write16(k.R[6], v)
}

func (k *KB11) pop() uint16 {
	val := k.read16(k.R[6])
	k.R[6] += 2
	return val
}

func ostr(z interface{}, n int) string {
	return fmt.Sprintf("%0"+fmt.Sprintf("%d", n)+"o", z)
}

var writedebug = fmt.Print

func (k *KB11) printstate() {
	writedebug(fmt.Sprintf("R0 %06o R1 %06o R2 %06o R3 %06o R4 %06o R5 %06o R6 %06o R7 %06o\n[", k.R[0], k.R[1], k.R[2], k.R[3], k.R[4], k.R[5], k.R[6], k.R[7]))
	if prevuser {
		writedebug("u")
	} else {
		writedebug("k")
	}
	if curuser {
		writedebug("U")
	} else {
		writedebug("K")
	}
	if k.PS&FLAGN != 0 {
		writedebug("N")
	} else {
		writedebug(" ")
	}
	if k.PS&FLAGZ != 0 {
		writedebug("Z")
	} else {
		writedebug(" ")
	}
	if k.PS&FLAGV != 0 {
		writedebug("V")
	} else {
		writedebug(" ")
	}
	if k.PS&FLAGC != 0 {
		writedebug("C")
	} else {
		writedebug(" ")
	}
	ia := k.decode(k.PC, false, curuser)
	instr := k.unibus.physread16(ia)
	writedebug("]  instr " + ostr(k.PC, 6) + ": " + ostr(instr, 6) + "   " + disasm(ia) + "\n")
}

type trap struct {
	num int
	msg string
}

func (t trap) String() string {
	return fmt.Sprintf("trap %06o occured: %s", t.num, t.msg)
}

func interrupt(vec, pri int) {
	var i int
	if vec&1 == 1 {
		panic("Thou darst calling interrupt() with an odd vector number?")
	}
	for ; i < len(interrupts); i++ {
		if interrupts[i].pri < pri {
			break
		}
	}
	for ; i < len(interrupts); i++ {
		if interrupts[i].vec >= vec {
			break
		}
	}
	// interrupts.splice(i, 0, {vec: vec, pri: pri});
	interrupts = append(interrupts[:i], append([]intr{{vec, pri}}, interrupts[i:]...)...)
}

func (k *KB11) handleinterrupt(vec int) {
	defer func() {
		trap := recover()
		switch trap := trap.(type) {
		case struct {
			num int
			msg string
		}:
			k.trapat(trap.num, trap.msg)
		case nil:
			break
		default:
			panic(trap)
		}
		k.R[7] = memory[vec>>1]
		k.PS = memory[(vec>>1)+1]
		if prevuser {
			k.PS |= (1 << 13) | (1 << 12)
		}
		waiting = false
	}()
	prev := k.PS
	k.switchmode(false)
	k.push(prev)
	k.push(k.R[7])
}

func (k *KB11) trapat(vec int, msg string) {
	var prev uint16
	defer func() {
		t := recover()
		switch t := t.(type) {
		case trap:
			writedebug("red stack trap!\n")
			memory[0] = k.R[7]
			memory[1] = prev
			vec = 4
			panic("fatal")
		case nil:
			break
		default:
			panic(t)
		}
		k.R[7] = memory[vec>>1]
		k.PS = memory[(vec>>1)+1]
		if prevuser {
			k.PS |= (1 << 13) | (1 << 12)
		}
		waiting = false
	}()
	if vec&1 == 1 {
		panic("Thou darst calling trapat() with an odd vector number?")
	}
	writedebug("trap " + ostr(vec, 6) + " occured: " + msg + "\n")
	k.printstate()

	prev = k.PS
	k.switchmode(false)
	k.push(prev)
	k.push(k.R[7])
}

func (k *KB11) aget(v uint16, l uint16) int {
	if (v&7) >= 6 || (v&010 != 0) {
		l = 2
	}
	if (v & 070) == 000 {
		return -int(v + 1)
	}
	var addr uint16
	switch v & 060 {
	case 000:
		v &= 7
		addr = k.R[v&7]
	case 020:
		addr = k.R[v&7]
		k.R[v&7] += l
	case 040:
		k.R[v&7] -= l
		addr = k.R[v&7]
	case 060:
		addr = k.fetch16()
		addr += k.R[v&7]
	}
	addr &= 0xFFFF
	if v&010 != 0 {
		addr = k.read16(addr)
	}
	return int(addr)
}

func (k *KB11) memread(a int, l uint16) uint16 {
	if a < 0 {
		r := uint8(-(a + 1))
		if l == 2 {
			return k.R[r&7]
		} else {
			return k.R[r&7] & 0xFF
		}
	}
	if l == 2 {
		return k.read16(uint16(a))
	}
	return k.read8(uint16(a))
}

func (k *KB11) memwrite(a int, l, v uint16) {
	if a < 0 {
		r := uint8(-(a + 1))
		if l == 2 {
			k.R[r&7] = v
		} else {
			k.R[r&7] &= 0xFF00
			k.R[r&7] |= v
		}
	} else if l == 2 {
		k.write16(uint16(a), v)
	} else {
		k.write8(uint16(a), v)
	}
}

func (k *KB11) branch(o int) {
	//printstate()
	if o&0x80 == 0x80 {
		o = -(((^o) + 1) & 0xFF)
	}
	o <<= 1
	k.R[7] += uint16(o)
}

func (k *KB11) step() {
	var max, maxp, msb uint16
	if waiting {
		select {
		case v, ok := <-k.unibus.cons.Input:
			if ok {
				k.unibus.cons.addchar(int(v))
			}
		default:
		}
		return
	}
	k.PC = k.R[7]
	ia := k.decode(k.PC, false, curuser)
	k.R[7] += 2
	if pr {
		k.printstate()
	}
	instr := k.unibus.physread16(ia)
	d := instr & 077
	s := (instr & 07700) >> 6
	l := 2 - (instr >> 15)
	o := int(instr) & 0xFF
	if l == 2 {
		max = 0xFFFF
		maxp = 0x7FFF
		msb = 0x8000
	} else {
		max = 0xFF
		maxp = 0x7F
		msb = 0x80
	}
	switch instr & 0070000 {
	case 0010000: // MOV
		// k.printstate()
		sa := k.aget(s, l)
		val := k.memread(sa, l)
		da := k.aget(d, l)
		k.PS &= 0xFFF1
		if val&msb == msb {
			k.PS |= FLAGN
		}
		if val == 0 {
			k.PS |= FLAGZ
		}
		if da < 0 && l == 1 {
			l = 2
			if val&msb == msb {
				val |= 0xFF00
			}
		}
		k.memwrite(da, l, val)
		return
	case 0020000: // CMP
		sa := k.aget(s, l)
		val1 := k.memread(sa, l)
		da := k.aget(d, l)
		val2 := k.memread(da, l)
		val := (val1 - val2) & max
		k.PS &= 0xFFF0
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&msb == msb {
			k.PS |= FLAGN
		}
		if (val1^val2)&msb == msb && !((val2^val)&msb == msb) {
			k.PS |= FLAGV
		}
		if val1 < val2 {
			k.PS |= FLAGC
		}
		return
	case 0030000: // BIT
		sa := k.aget(s, l)
		val1 := k.memread(sa, l)
		da := k.aget(d, l)
		val2 := k.memread(da, l)
		val := val1 & val2
		k.PS &= 0xFFF1
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&msb == msb {
			k.PS |= FLAGN
		}
		return
	case 0040000: // BIC
		sa := k.aget(s, l)
		val1 := k.memread(sa, l)
		da := k.aget(d, l)
		val2 := k.memread(da, l)
		val := (max ^ val1) & val2
		k.PS &= 0xFFF1
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&msb != 0 {
			k.PS |= FLAGN
		}
		k.memwrite(da, l, val)
		return
	case 0050000: // BIS
		sa := k.aget(s, l)
		val1 := k.memread(sa, l)
		da := k.aget(d, l)
		val2 := k.memread(da, l)
		val := val1 | val2
		k.PS &= 0xFFF1
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&msb == msb {
			k.PS |= FLAGN
		}
		k.memwrite(da, l, val)
		return
	}
	switch instr & 0170000 {
	case 0060000: // ADD
		sa := k.aget(s, 2)
		val1 := k.memread(sa, 2)
		da := k.aget(d, 2)
		val2 := k.memread(da, 2)
		val := (val1 + val2) & 0xFFFF
		k.PS &= 0xFFF0
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&0x8000 == 0x8000 {
			k.PS |= FLAGN
		}
		if !((val1^val2)&0x8000 == 0x8000) && ((val2^val)&0x8000 == 0x8000) {
			k.PS |= FLAGV
		}
		if val1+val2 >= 0xFFFF {
			k.PS |= FLAGC
		}
		k.memwrite(da, 2, val)
		return
	case 0160000: // SUB
		sa := k.aget(s, 2)
		val1 := k.memread(sa, 2)
		da := k.aget(d, 2)
		val2 := k.memread(da, 2)
		val := (val2 - val1) & 0xFFFF
		k.PS &= 0xFFF0
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&0x8000 == 0x8000 {
			k.PS |= FLAGN
		}
		if ((val1^val2)&0x8000 == 0x8000) && !((val2^val)&0x8000 == 0x8000) {
			k.PS |= FLAGV
		}
		if val1 > val2 {
			k.PS |= FLAGC
		}
		k.memwrite(da, 2, val)
		return
	}
	switch instr & 0177000 {
	case 0004000: // JSR
		val := k.aget(d, l)
		if val < 0 {
			break
		}
		k.push(k.R[s&7])
		k.R[s&7] = k.R[7]
		k.R[7] = uint16(val)
		return
	case 0070000: // MUL
		val1 := int(k.R[s&7])
		if val1&0x8000 == 0x8000 {
			val1 = -((0xFFFF ^ val1) + 1)
		}
		da := k.aget(d, l)
		val2 := int(k.memread(da, 2))
		if val2&0x8000 == 0x8000 {
			val2 = -((0xFFFF ^ val2) + 1)
		}
		val := val1 * val2
		k.R[s&7] = uint16((val & 0xFFFF0000) >> 16)
		k.R[(s&7)|1] = uint16(val & 0xFFFF)
		k.PS &= 0xFFF0
		if val&0x80000000 == 0x80000000 {
			k.PS |= FLAGN
		}
		if val&0xFFFFFFFF == 0 {
			k.PS |= FLAGZ
		}
		if val < (1<<15) || val >= ((1<<15)-1) {
			k.PS |= FLAGC
		}
		return
	case 0071000: // DIV
		val1 := int(k.R[s&7]<<16) | int(k.R[(s&7)|1])
		da := k.aget(d, l)
		val2 := int(k.memread(da, 2))
		k.PS &= 0xFFF0
		if val2 == 0 {
			k.PS |= FLAGC
			return
		}
		if val1/val2 >= 0x10000 {
			k.PS |= FLAGV
			return
		}
		k.R[s&7] = uint16((val1 / val2) & 0xFFFF)
		k.R[(s&7)|1] = uint16((val1 % val2) & 0xFFFF)
		if k.R[s&7] == 0 {
			k.PS |= FLAGZ
		}
		if k.R[s&7]&0100000 == 0100000 {
			k.PS |= FLAGN
		}
		if val1 == 0 {
			k.PS |= FLAGV
		}
		return
	case 0072000: // ASH
		val1 := k.R[s&7]
		da := k.aget(d, 2)
		val2 := k.memread(da, 2) & 077
		k.PS &= 0xFFF0
		var val uint16
		if val2&040 != 0 {
			val2 = (077 ^ val2) + 1
			if val1&0100000 == 0100000 {
				val = 0xFFFF ^ (0xFFFF >> val2)
				val |= val1 >> val2
			} else {
				val = val1 >> val2
			}
			shift := uint16(1) << (val2 - 1)
			if val1&shift == shift {
				k.PS |= FLAGC
			}
		} else {
			val = (val1 << val2) & 0xFFFF
			shift := uint16(1) << (16 - val2)
			if val1&shift == shift {
				k.PS |= FLAGC
			}
		}
		k.R[s&7] = uint16(val)
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&0100000 == 0100000 {
			k.PS |= FLAGN
		}
		if xor16(val&0100000, val1&0100000) != 0 {
			k.PS |= FLAGV
		}
		return
	case 0073000: // ASHC
		val1 := int(k.R[s&7]<<16) | int(k.R[(s&7)|1])
		da := k.aget(d, 2)
		val2 := k.memread(da, 2) & 077
		k.PS &= 0xFFF0
		var val int
		if val2&040 != 0 {
			val2 = (077 ^ val2) + 1
			if val1&0x80000000 == 0x8000000 {
				val = 0xFFFFFFFF ^ (0xFFFFFFFF >> val2)
				val |= val1 >> val2
			} else {
				val = val1 >> val2
			}
			if val1&(1<<(val2-1)) != 0 {
				k.PS |= FLAGC
			}
		} else {
			val = (val1 << val2) & 0xFFFFFFFF
			if val1&(1<<(32-val2)) != 0 {
				k.PS |= FLAGC
			}
		}
		k.R[s&7] = uint16(val >> 16)
		k.R[(s&7)|1] = uint16(val & 0xFFFF)
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&0x80000000 != 0 {
			k.PS |= FLAGN
		}
		if xor(val&0x80000000, val1&0x80000000) != 0 {
			k.PS |= FLAGV
		}
		return
	case 0074000: // XOR
		val1 := k.R[s&7]
		da := k.aget(d, 2)
		val2 := k.memread(da, 2)
		val := val1 ^ val2
		k.PS &= 0xFFF1
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&0x8000 == 0x8000 {
			k.PS |= FLAGZ
		}
		k.memwrite(da, 2, val)
		return
	case 0077000: // SOB
		k.R[s&7]--
		if k.R[s&7] != 0 {
			o &= 077
			o <<= 1
			k.R[7] -= uint16(o)
		}
		return
	}
	switch instr & 0077700 {
	case 0005000: // CLR
		k.PS &= 0xFFF0
		k.PS |= FLAGZ
		da := k.aget(d, l)
		k.memwrite(da, l, 0)
		return
	case 0005100: // COM
		da := k.aget(d, l)
		val := k.memread(da, l) ^ max
		k.PS &= 0xFFF0
		k.PS |= FLAGC
		if val&msb == msb {
			k.PS |= FLAGN
		}
		if val == 0 {
			k.PS |= FLAGZ
		}
		k.memwrite(da, l, val)
		return
	case 0005200: // INC
		da := k.aget(d, l)
		val := (k.memread(da, l) + 1) & max
		k.PS &= 0xFFF1
		if val&msb == msb {
			k.PS |= FLAGN | FLAGV
		}
		if val == 0 {
			k.PS |= FLAGZ
		}
		k.memwrite(da, l, val)
		return
	case 0005300: // DEC
		da := k.aget(d, l)
		val := (k.memread(da, l) - 1) & max
		k.PS &= 0xFFF1
		if val&msb == msb {
			k.PS |= FLAGN
		}
		if val == maxp {
			k.PS |= FLAGV
		}
		if val == 0 {
			k.PS |= FLAGZ
		}
		k.memwrite(da, l, val)
		return
	case 0005400: // NEG
		da := k.aget(d, l)
		val := (-k.memread(da, l)) & max
		k.PS &= 0xFFF0
		if val&msb == msb {
			k.PS |= FLAGN
		}
		if val == 0 {
			k.PS |= FLAGZ
		} else {
			k.PS |= FLAGC
		}
		if val == 0x8000 {
			k.PS |= FLAGV
		}
		k.memwrite(da, l, val)
		return
	case 0005500: // ADC
		da := k.aget(d, l)
		val := k.memread(da, l)
		if k.PS&FLAGC == FLAGC {
			k.PS &= 0xFFF0
			if (val+1)&msb == msb {
				k.PS |= FLAGN
			}
			if val == max {
				k.PS |= FLAGZ
			}
			if val == 0077777 {
				k.PS |= FLAGV
			}
			if val == 0177777 {
				k.PS |= FLAGC
			}
			k.memwrite(da, l, (val+1)&max)
		} else {
			k.PS &= 0xFFF0
			if val&msb == msb {
				k.PS |= FLAGN
			}
			if val == 0 {
				k.PS |= FLAGZ
			}
		}
		return
	case 0005600: // SBC
		da := k.aget(d, l)
		val := k.memread(da, l)
		if k.PS&FLAGC == FLAGC {
			k.PS &= 0xFFF0
			if (val-1)&msb == msb {
				k.PS |= FLAGN
			}
			if val == 1 {
				k.PS |= FLAGZ
			}
			if val != 0 {
				k.PS |= FLAGC
			}
			if val == 0100000 {
				k.PS |= FLAGV
			}
			k.memwrite(da, l, (val-1)&max)
		} else {
			k.PS &= 0xFFF0
			if val&msb == msb {
				k.PS |= FLAGN
			}
			if val == 0 {
				k.PS |= FLAGZ
			}
			if val == 0100000 {
				k.PS |= FLAGV
			}
			k.PS |= FLAGC
		}
		return
	case 0005700: // TST
		da := k.aget(d, l)
		val := k.memread(da, l)
		k.PS &= 0xFFF0
		if val&msb == msb {
			k.PS |= FLAGN
		}
		if val == 0 {
			k.PS |= FLAGZ
		}
		return
	case 0006000: // ROR
		da := k.aget(d, l)
		val := k.memread(da, l)
		if k.PS&FLAGC == FLAGC {
			val |= max + 1
		}
		k.PS &= 0xFFF0
		if val&1 == 1 {
			k.PS |= FLAGC
		}
		if val&(max+1) != 0 {
			k.PS |= FLAGN
		}
		if !(val&max != 0) {
			k.PS |= FLAGZ
		}
		if xor16(val&1, val&(max+1)) != 0 {
			k.PS |= FLAGV
		}
		val >>= 1
		k.memwrite(da, l, val)
		return
	case 0006100: // ROL
		da := k.aget(d, l)
		val := k.memread(da, l) << 1
		if k.PS&FLAGC == FLAGC {
			val |= 1
		}
		k.PS &= 0xFFF0
		if val&(max+1) != 0 {
			k.PS |= FLAGC
		}
		if val&msb == msb {
			k.PS |= FLAGN
		}
		if !(val&max != 0) {
			k.PS |= FLAGZ
		}
		if (val^(val>>1))&msb != 0 {
			k.PS |= FLAGV
		}
		val &= max
		k.memwrite(da, l, val)
		return
	case 0006200: // ASR
		da := k.aget(d, l)
		val := k.memread(da, l)
		k.PS &= 0xFFF0
		if val&1 == 1 {
			k.PS |= FLAGC
		}
		if val&msb == msb {
			k.PS |= FLAGN
		}
		if xor16(val&msb, val&1) != 0 {
			k.PS |= FLAGV
		}
		val = (val & msb) | (val >> 1)
		if val == 0 {
			k.PS |= FLAGZ
		}
		k.memwrite(da, l, val)
		return
	case 0006300: // ASL
		da := k.aget(d, l)
		val := k.memread(da, l)
		k.PS &= 0xFFF0
		if val&msb == msb {
			k.PS |= FLAGC
		}
		if val&(msb>>1) != 0 {
			k.PS |= FLAGN
		}
		if (val^(val<<1))&msb != 0 {
			k.PS |= FLAGV
		}
		val = (val << 1) & max
		if val == 0 {
			k.PS |= FLAGZ
		}
		k.memwrite(da, l, val)
		return
	case 0006700: // SXT
		da := k.aget(d, l)
		if k.PS&FLAGN == FLAGN {
			k.memwrite(da, l, max)
		} else {
			k.PS |= FLAGZ
			k.memwrite(da, l, 0)
		}
		return
	}
	switch instr & 0177700 {
	case 0000100: // JMP
		val := k.aget(d, 2)
		if val < 0 {
			panic("whoa!")
			break
		}
		k.R[7] = uint16(val)
		return
	case 0000300: // SWAB
		da := k.aget(d, l)
		val := k.memread(da, l)
		val = ((val >> 8) | (val << 8)) & 0xFFFF
		k.PS &= 0xFFF0
		if (val & 0xFF) == 0 {
			k.PS |= FLAGZ
		}
		if val&0x80 == 0x80 {
			k.PS |= FLAGN
		}
		k.memwrite(da, l, val)
		return
	case 0006400: // MARK
		k.R[6] = k.R[7] + (instr&077)<<1
		k.R[7] = k.R[5]
		k.R[5] = k.pop()
		break
	case 0006500: // MFPI
		var val uint16
		da := k.aget(d, 2)
		switch {
		case da == -7:
			// val = (curuser == prevuser) ? R[6] : (prevuser ? k.USP : KSP);
			if curuser == prevuser {
				val = k.R[6]
			} else {
				if prevuser {
					val = k.USP
				} else {
					val = k.KSP
				}
			}
		case da < 0:
			panic("invalid MFPI instruction")
		default:
			val = k.unibus.physread16(k.decode(uint16(da), false, prevuser))
		}
		k.push(val)
		k.PS &= 0xFFF0
		k.PS |= FLAGC
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&0x8000 == 0x800 {
			k.PS |= FLAGN
		}
		return
	case 0006600: // MTPI
		da := k.aget(d, 2)
		val := k.pop()
		switch {
		case da == -7:
			if curuser == prevuser {
				k.R[6] = val
			} else {
				if prevuser {
					k.USP = val
				} else {
					k.KSP = val
				}
			}
		case da < 0:
			panic("invalid MTPI instrution")
		default:
			sa := k.decode(uint16(da), true, prevuser)
			k.unibus.physwrite16(sa, val)
		}
		k.PS &= 0xFFF0
		k.PS |= FLAGC
		if val == 0 {
			k.PS |= FLAGZ
		}
		if val&0x8000 == 0x8000 {
			k.PS |= FLAGN
		}
		return
	}
	if (instr & 0177770) == 0000200 { // RTS
		k.R[7] = k.R[d&7]
		k.R[d&7] = k.pop()
		return
	}
	switch instr & 0177400 {
	case 0000400:
		k.branch(o)
		return
	case 0001000:
		if !(k.PS&FLAGZ == FLAGZ) {
			k.branch(o)
		}
		return
	case 0001400:
		if k.PS&FLAGZ == FLAGZ {
			k.branch(o)
		}
		return
	case 0002000:
		if !(xor16(k.PS&FLAGN, k.PS&FLAGV) != 0) {
			k.branch(o)
		}
		return
	case 0002400:
		if xor16(k.PS&FLAGN, k.PS&FLAGV) != 0 {
			k.branch(o)
		}
		return
	case 0003000:
		if !(xor16(k.PS&FLAGN, k.PS&FLAGV) != 0) && !(k.PS&FLAGZ == FLAGZ) {
			k.branch(o)
		}
		return
	case 0003400:
		if xor16(k.PS&FLAGN, k.PS&FLAGV) != 0 || (k.PS&FLAGZ == FLAGZ) {
			k.branch(o)
		}
		return
	case 0100000:
		if k.PS&FLAGN == 0 {
			k.branch(o)
		}
		return
	case 0100400:
		if k.PS&FLAGN == FLAGN {
			k.branch(o)
		}
		return
	case 0101000:
		if !(k.PS&FLAGC == FLAGC) && !(k.PS&FLAGZ == FLAGZ) {
			k.branch(o)
		}
		return
	case 0101400:
		if (k.PS&FLAGC == FLAGC) || (k.PS&FLAGZ == FLAGZ) {
			k.branch(o)
		}
		return
	case 0102000:
		if !(k.PS&FLAGV == FLAGV) {
			k.branch(o)
		}
		return
	case 0102400:
		if k.PS&FLAGV == FLAGV {
			k.branch(o)
		}
		return
	case 0103000:
		if !(k.PS&FLAGC == FLAGC) {
			k.branch(o)
		}
		return
	case 0103400:
		if k.PS&FLAGC == FLAGC {
			k.branch(o)
		}
		return
	}
	if (instr&0177000) == 0104000 || instr == 3 || instr == 4 { // EMT TRAP IOT BPT
		var vec uint16
		switch {
		case (instr & 0177400) == 0104000:
			vec = 030
		case (instr & 0177400) == 0104400:
			vec = 034
		case instr == 3:
			vec = 014
		default:
			vec = 020
		}
		prev := k.PS
		k.switchmode(false)
		k.push(prev)
		k.push(k.R[7])
		k.R[7] = memory[vec>>1]
		k.PS = memory[(vec>>1)+1]
		if prevuser {
			k.PS |= (1 << 13) | (1 << 12)
		}
		return
	}
	if (instr & 0177740) == 0240 { // CL?, SE?
		if instr&020 == 020 {
			k.PS |= instr & 017
		} else {
			k.PS &= ^(instr & 017)
		}
		return
	}
	switch instr {
	case 0000000: // HALT
		if curuser {
			break
		}
		writedebug("HALT\n")
		k.printstate()
		panic("HALT")
		return
	case 0000001: // WAIT
		if curuser {
			break
		}
		//println("WAIT")
		waiting = true
		return
	case 0000002: // RTI
		fallthrough
	case 0000006: // RTT
		k.R[7] = k.pop()
		val := k.pop()
		if curuser {
			val &= 047
			val |= k.PS & 0177730
		}
		k.unibus.physwrite16(0777776, val)
		return
	case 0000005: // RESET
		if curuser {
			return
		}
		k.unibus.cons.clearterminal()
		k.unibus.rk.rkreset()
		return
	case 0170011: // SETD ; not needed by UNIX, but used; therefore ignored
		return
	}
	fmt.Println(ia, disasm(ia))
	panic(trap{INTINVAL, "invalid instruction"})
}

func (k *KB11) Reset() {
	for i := 0; i < 7; i++ {
		k.R[i] = 0
	}
	k.PS = 0
	k.PC = 0
	k.KSP = 0
	k.USP = 0
	k.Input = make(chan uint8) // unix\n
	curuser = false
	prevuser = false
	k.SR0 = 0
	k.unibus.LKS = 1 << 7
	for i := 0; i < len(memory); i++ {
		memory[i] = 0
	}
	for i := 0; i < len(bootrom); i++ {
		memory[01000+i] = bootrom[i]
	}
	for i := 0; i < 16; i++ {
		pages[i] = createpage(0, 0)
	}
	k.R[7] = 02002
	k.unibus.cons.clearterminal()
	k.unibus.cons.Input = k.Input
	k.unibus.rk.rkreset()
	clkcounter = 0
	waiting = false
}

func (k *KB11) Step() {
	k.onestep()
	k.unibus.rk.Step()
	k.unibus.cons.Step(k)
}

func (k *KB11) onestep() {
	defer func() {
		t := recover()
		switch t := t.(type) {
		case trap:
			k.trapat(t.num, t.msg)
		case nil:
			// ignore
		default:
			panic(t)
		}
	}()

	k.step()
	if len(interrupts) > 0 && interrupts[0].pri >= ((int(k.PS)>>5)&7) {
		//fmt.Printf("IRQ: %06o\n", interrupts[0].vec)
		k.handleinterrupt(interrupts[0].vec)
		interrupts = interrupts[1:]
	}
	clkcounter++
	if clkcounter >= 40000 {
		clkcounter = 0
		k.unibus.LKS |= (1 << 7)
		if k.unibus.LKS&(1<<6) != 0 {
			interrupt(INTCLOCK, 6)
		}
	}
}

func New() *KB11 {
	var cpu KB11
	var unibus Unibus
	var rk RK05
	var cons Console
	cpu.unibus = &unibus
	unibus.cpu = &cpu
	unibus.rk = &rk
	unibus.cons = &cons
	rk.rkinit()
	cpu.Reset()
	return &cpu
}
