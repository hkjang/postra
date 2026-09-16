package application

// nilToEmpty applies only to fields whose successful wire contract is a list.
// Optional objects, absent bodies and error results keep their meaning; do not
// recursively rewrite arbitrary JSON null values.
func nilToEmpty[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func listResult[T any](items []T, err error) ([]T, error) {
	if err == nil {
		items = nilToEmpty(items)
	}
	return items, err
}
