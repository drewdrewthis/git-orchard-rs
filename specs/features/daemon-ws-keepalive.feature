Feature: daemon graphql-transport-ws keepalive pings (#788)
  As the orchard-sidebar push lane
  I want the daemon to send graphql-transport-ws ping frames on idle subscriptions
  So that a quiet tmux subscription is not torn down every ~30s by the client read deadline

  Background:
    Given the orchard daemon serves GraphQL websockets at "127.0.0.1:7777/graphql"
    And the websocket transport sets both "KeepAlivePingInterval" and "PingPongInterval" to 10s
    And "KeepAlivePingInterval" drives only the legacy "graphql-ws" keepalive frame
    And "PingPongInterval" drives the "graphql-transport-ws" ping frame

  @e2e
  Scenario: idle graphql-transport-ws subscription stays connected
    Given a daemon with an idle tmux subscription over "graphql-transport-ws"
    When 5 minutes pass with no tmux activity
    Then the client receives "ping" frames at roughly 10s intervals
    And the client logs zero "i/o timeout" reconnects for that subscription

  @unit
  Scenario: ping arrives within 15s on an idle subscription
    Given a graphql-transport-ws client that sends "connection_init" and reads "connection_ack"
    And it starts a subscription that emits no data
    When it waits with no further writes
    Then a frame of type "ping" arrives within 15s
