cmake_minimum_required(VERSION 3.16)
project(bswisp_scheduler CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_library(bswisp_core STATIC
  src/json.cpp
  src/model.cpp
  src/airtime.cpp
)
target_include_directories(bswisp_core PUBLIC include)
target_compile_options(bswisp_core PRIVATE -Wall -Wextra -Wpedantic)

add_executable(bswisp-solver src/main.cpp)
target_link_libraries(bswisp-solver PRIVATE bswisp_core)
target_compile_options(bswisp-solver PRIVATE -Wall -Wextra -Wpedantic)

enable_testing()
add_executable(test_airtime tests/test_airtime.cpp)
target_link_libraries(test_airtime PRIVATE bswisp_core)
target_compile_options(test_airtime PRIVATE -Wall -Wextra -Wpedantic)
add_test(NAME airtime COMMAND test_airtime)
