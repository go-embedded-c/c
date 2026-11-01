#include <stdio.h>
#include <stdlib.h>
#include <string.h>

struct node { int v; struct node *next; };

static unsigned long fib(unsigned n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); }

static int cmp(const void *a, const void *b)
{
	int x = *(const int *)a, y = *(const int *)b;
	return (x > y) - (x < y);
}

int main(void)
{
	struct node *head = NULL;
	for (int i = 0; i < 1000; i++) {
		struct node *n = malloc(sizeof *n);
		n->v = i * 7 % 101;
		n->next = head;
		head = n;
	}
	long sum = 0;
	int arr[10], k = 0;
	for (struct node *n = head; n; n = n->next) {
		sum += n->v;
		if (k < 10) arr[k++] = n->v;
	}
	qsort(arr, 10, sizeof arr[0], cmp);
	printf("fib(24)=%lu sum=%ld\n", fib(24), sum);
	for (int i = 0; i < 10; i++) printf("%d ", arr[i]);
	printf("\n");
	while (head) { struct node *n = head->next; free(head); head = n; }
	char *s = strdup("heap string");
	printf("%s %zu\n", s, strlen(s));
	free(s);
	return 0;
}
