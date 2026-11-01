/* abort() in wasi-libc ends the guest; record what the engine reports. */
#include <stdio.h>
#include <stdlib.h>

int main(void)
{
	printf("before\n");
	fflush(stdout);
	abort();
}
