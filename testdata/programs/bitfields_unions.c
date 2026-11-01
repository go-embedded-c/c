#include <stdio.h>
#include <string.h>

struct flags {
	unsigned a : 3;
	unsigned b : 5;
	int c : 4;
	unsigned d : 20;
};

union pun { float f; unsigned u; unsigned char b[4]; };

int main(void)
{
	struct flags f;
	memset(&f, 0, sizeof f);
	f.a = 9;       /* wraps to 1 */
	f.b = 31;
	f.c = -3;
	f.d = 0xfffff;
	printf("%u %u %d %u size=%zu\n", f.a, f.b, f.c, f.d, sizeof f);
	union pun p;
	p.f = 1.0f;
	printf("%08x %02x%02x%02x%02x\n", p.u, p.b[0], p.b[1], p.b[2], p.b[3]);
	return 0;
}
