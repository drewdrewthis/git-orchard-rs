/// Maps a digit character `'1'`–`'9'` to a 0-based list index.
/// Returns `None` if the character is not in `'1'`–`'9'` or if the resulting
/// index is out of bounds for a list of `item_count` items.
pub fn cursor_index_from_digit(input: char, item_count: usize) -> Option<usize> {
    if !input.is_ascii_digit() || input == '0' {
        return None;
    }
    let idx = (input as usize) - ('1' as usize); // '1'→0, '2'→1, …
    if idx >= item_count {
        return None;
    }
    Some(idx)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn digit_one_maps_to_zero() {
        assert_eq!(cursor_index_from_digit('1', 5), Some(0));
    }

    #[test]
    fn digit_nine_maps_to_eight() {
        assert_eq!(cursor_index_from_digit('9', 9), Some(8));
    }

    #[test]
    fn digit_out_of_bounds_returns_none() {
        assert_eq!(cursor_index_from_digit('5', 4), None);
    }

    #[test]
    fn zero_returns_none() {
        assert_eq!(cursor_index_from_digit('0', 10), None);
    }

    #[test]
    fn non_digit_returns_none() {
        assert_eq!(cursor_index_from_digit('a', 5), None);
    }

    #[test]
    fn empty_list_returns_none() {
        assert_eq!(cursor_index_from_digit('1', 0), None);
    }

    #[test]
    fn digit_two_in_three_item_list() {
        assert_eq!(cursor_index_from_digit('2', 3), Some(1));
    }
}
