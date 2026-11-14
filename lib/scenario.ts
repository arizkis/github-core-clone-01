const MASK = 0xffffffffffffffffn;
const SEED = 0x6a09e667f3bcc909n;
const FIRST = ['ada','bram','cora','dmitri','elena','felix','greta','hugo','iris','jonas','kira','lars','mira','nico','olga','pavel','quinn','rosa','sven','tess','ulla','vito','wren','yara','zane'];
const LAST = ['anders','bauer','costa','dahl','eriksen','fischer','garcia','holm','iversen','jansen','keller','lind','moreau','novak','olsen','peters','quist','rossi','sato','turi','ulrich','vega','weber','xu','yilmaz','zimmer'];

function fnv(text: string): bigint {
  let hash = 0xcbf29ce484222325n;
  for (let index = 0; index < text.length; index += 1) hash = ((hash ^ BigInt(text.charCodeAt(index)!)) * 0x100000001b3n) & MASK;
  return hash;
}
function mix(value: bigint): bigint {
  let value64 = (value + 0x9e3779b97f4a7c15n) & MASK;
  value64 = ((value64 ^ (value64 >> 30n)) * 0xbf58476d1ce4e5b9n) & MASK;
  value64 = ((value64 ^ (value64 >> 27n)) * 0x94d049bb133111ebn) & MASK;
  return (value64 ^ (value64 >> 31n)) & MASK;
}
class Rng {
  private state: bigint;
  constructor(seed: bigint) { this.state = (seed & MASK) || SEED; }
  nextInt(limit: number): number {
    const ceiling = 4294967296 - (4294967296 % limit);
    for (;;) { const value = Number(this.next() >> 32n); if (value < ceiling) return value % limit; }
  }
  private next(): bigint {
    let value = this.state;
    value = (value ^ (value >> 12n)) & MASK;
    value = (value ^ (value << 25n)) & MASK;
    value = (value ^ (value >> 27n)) & MASK;
    this.state = value;
    return (value * 0x2545f4914f6cdd1dn) & MASK;
  }
}
function users(): string[] {
  const rng = new Rng(mix(SEED ^ fnv('users'))), used = new Set<string>(), result: string[] = [];
  for (let index = 0; index < 12; index += 1) {
    const base = `${FIRST[rng.nextInt(FIRST.length)]}${LAST[rng.nextInt(LAST.length)]}`;
    let login = base, suffix = 2;
    while (used.has(login)) login = `${base}-${suffix++}`;
    used.add(login); result.push(login);
  }
  return result;
}
const generated = users();
export const viewerLogin = generated[0]!;
export const strangerLogin = generated[2]!;

export function tokenIsAuthorized(header: string | undefined): boolean {
  const match = /^Bearer\s+(\S+)$/i.exec(header ?? '');
  if (!match) return false;
  const expected = process.env.MAKEFUN_SHOP_TOKEN;
  return !expected || match[1] === expected;
}
