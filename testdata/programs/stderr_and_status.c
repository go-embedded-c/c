#include <stdio.h>

int main(void)
{
	fprintf(stdout, "to stdout\n");
	fprintf(stderr, "to stderr\n");
	fflush(stdout);
	return 7;
}
